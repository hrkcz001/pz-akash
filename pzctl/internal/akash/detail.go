package akash

// The GET /v1/deployments/{dseq} response, which is the only endpoint that
// answers the three questions the controller actually asks: is the lease still
// alive, where can players reach it, and how much escrow is left.
//
// Every field here was read off a live deployment rather than a document, and two
// details from that reading are worth keeping in mind:
//
//   - Money arrives as a decimal string: "34.000000000000000000". Hence Num.
//   - The IP entries use capitalised JSON keys ("IP", "Port"), unlike everything
//     else in the API. Go's decoder is case-insensitive so it would work either
//     way; the tags below say what the wire actually holds.
//
// A lease also carries reason: "lease_closed_invalid" while perfectly active — it
// is the zero value of an enum, not a statement about the lease. Nothing here
// reads it, on purpose.

import (
	"encoding/json"

	"github.com/hrkcz001/pz-akash/pzctl/internal/denom"
)

// Lease states as the API reports them.
const (
	leaseStateActive = "active"
	deployStateOpen  = "open"
)

// deploymentDetail is GET /v1/deployments/{dseq}.
type deploymentDetail struct {
	Data struct {
		Deployment struct {
			ID struct {
				Owner string          `json:"owner"`
				DSeq  json.RawMessage `json:"dseq"`
			} `json:"id"`
			State string `json:"state"`
		} `json:"deployment"`
		Leases        []leaseDetail `json:"leases"`
		EscrowAccount escrowAccount `json:"escrow_account"`
	} `json:"data"`
}

// leaseDetail is one lease with its provider-reported status.
type leaseDetail struct {
	ID struct {
		Owner    string          `json:"owner"`
		DSeq     json.RawMessage `json:"dseq"`
		GSeq     int             `json:"gseq"`
		OSeq     int             `json:"oseq"`
		Provider string          `json:"provider"`
	} `json:"id"`
	State string `json:"state"`
	Price struct {
		Denom  string `json:"denom"`
		Amount Num    `json:"amount"`
	} `json:"price"`
	Status leaseStatus `json:"status"`
}

// leaseStatus is what the provider reports about the running workload. It is
// absent from the list endpoint and present here, which is why adoption needs a
// per-deployment call.
type leaseStatus struct {
	Services map[string]serviceStatus `json:"services"`
	// ForwardedPorts is how a shared-endpoint service is reached: the provider's
	// own hostname and a port it assigned. The controller is deployed this way.
	ForwardedPorts map[string][]forwardedPort `json:"forwarded_ports"`
	// IPs is how a dedicated-IP service is reached. The game server is deployed
	// this way, because players connect to an address in a DNS record.
	IPs map[string][]leaseIP `json:"ips"`
}

type serviceStatus struct {
	Name          string `json:"name"`
	Available     int    `json:"available"`
	Total         int    `json:"total"`
	Replicas      int    `json:"replicas"`
	ReadyReplicas int    `json:"ready_replicas"`
	// URIs is the HTTP ingress hostname list, populated only when a service
	// exposes `as: 80`. Our controller exposes its own port instead, so this is
	// normally empty — but controller.http_port is config, an operator may set it
	// to 80, and v1's URL resolver checked uris first for exactly that reason.
	URIs []string `json:"uris"`
}

// Ready reports whether at least one replica is serving. Akash counts replicas
// like Kubernetes: ready_replicas is the number that passed their probes.
func (s serviceStatus) Ready() bool { return s.ReadyReplicas > 0 || s.Available > 0 }

type forwardedPort struct {
	Host string `json:"host"`
	// Port is the container port; ExternalPort is what the world connects to.
	Port         int    `json:"port"`
	ExternalPort int    `json:"externalPort"`
	Proto        string `json:"proto"`
	Name         string `json:"name"`
}

type leaseIP struct {
	IP           string `json:"IP"`
	Port         int    `json:"Port"`
	ExternalPort int    `json:"ExternalPort"`
	Protocol     string `json:"Protocol"`
}

// escrowAccount is the deployment's escrow. funds is what is left; transferred is
// what the provider has already been paid.
type escrowAccount struct {
	State struct {
		State       string `json:"state"`
		Funds       []coin `json:"funds"`
		Transferred []coin `json:"transferred"`
	} `json:"state"`
}

type coin struct {
	Denom  string `json:"denom"`
	Amount Num    `json:"amount"`
}

// pick returns the amount in d from a coin list, and whether it was there. A
// list holding only denominations we cannot price is not zero funds, it is an
// unknown balance, and topping up against a wrong zero is how you spend real
// money on a deployment that needed nothing.
func pick(coins []coin, d string) (float64, bool) {
	for _, c := range coins {
		if denom.Normalize(c.Denom) == denom.Normalize(d) {
			return c.Amount.F(), true
		}
	}
	return 0, false
}

// deploymentList is GET /v1/deployments, used by Adopt.
//
// data is an object holding deployments and pagination — not, as this type first
// had it, an array of pages. The difference is invisible until the call is made
// against the real API, which is why it survived a whole test suite: a fake is only
// ever as right as whoever wrote it.
type deploymentList struct {
	Data struct {
		Deployments []struct {
			Deployment struct {
				ID struct {
					Owner string          `json:"owner"`
					DSeq  json.RawMessage `json:"dseq"`
				} `json:"id"`
				State     string `json:"state"`
				CreatedAt string `json:"created_at"`
			} `json:"deployment"`
			// Leases is deliberately absent. The list response carries one, and on a
			// wallet holding two deployments it carried each deployment paired with the
			// *other* one's lease — same gseq and oseq, a dseq belonging to its
			// neighbour. Whether that is a zip bug in Console or an ordering nobody
			// documented, a lease read from here identifies the wrong deployment, and
			// the one thing this list is used for is deciding what to close.
			//
			// Everything that needs a lease asks /v1/deployments/{dseq}, where the
			// answer is scoped to the deployment by construction.
		} `json:"deployments"`
		Pagination struct {
			Total   int  `json:"total"`
			Skip    int  `json:"skip"`
			Limit   int  `json:"limit"`
			HasMore bool `json:"hasMore"`
		} `json:"pagination"`
	} `json:"data"`
}

// jwtResponse is POST /v1/create-jwt-token.
type jwtResponse struct {
	Data struct {
		Token string `json:"token"`
	} `json:"data"`
}

// providerStatusResponse is the provider's own lease status endpoint, which
// answers before the Console API has caught up. Same services/ips shape.
type providerStatusResponse struct {
	Services       map[string]serviceStatus   `json:"services"`
	ForwardedPorts map[string][]forwardedPort `json:"forwarded_ports"`
	IPs            map[string][]leaseIP       `json:"ips"`
}
