package akash

// Finding our own address from the inside.
//
// The controller cannot be told where it is. Its URL is decided by the provider at
// lease time, after the SDL that would have carried it was already submitted, so the
// container starts with no idea what host:port the world can reach it on. v1 lived
// with that by routing everything through the DNS name, which works right up to the
// point where a request is large.
//
// It is discoverable, though, and by exactly the mechanism Adopt already uses: list
// the deployments on this wallet, find the one running the controller service, and
// read the address the provider published for it.

import (
	"context"
	"fmt"
	"strings"

	"github.com/hrkcz001/pz-akash/pzctl/internal/sdl"
)

// SelfURL returns the provider's own address for the controller service, or "" with
// no error when nothing on this wallet is running one.
//
// The empty-and-no-error case is the normal one outside a lease: `pzctl` on a laptop,
// a dry run, a controller whose lease has not gone active yet. The caller's fallback
// is the DNS name it was already using, so not-found must not read as broken.
//
// More than one candidate is refused rather than guessed. Two controllers on one
// wallet means one of them is about to be replaced, and handing the agent the
// address of the one that is going away would send a backup upload into a lease that
// is closing — the upload fails, the world's backup does not exist, and the report
// says the controller is fine. An honest "" costs one large request through
// Cloudflare instead.
func (d *Driver) SelfURL(ctx context.Context) (string, error) {
	var list deploymentList
	if err := d.Client.do(ctx, "GET", "/v1/deployments?limit=1000", nil, &list); err != nil {
		return "", fmt.Errorf("listing deployments to find our own address: %w", err)
	}
	if list.Data.Pagination.HasMore {
		d.Logf("akash: WARNING the deployment list is paginated (%d total) while looking for our own address",
			list.Data.Pagination.Total)
	}

	var found []string
	for _, dep := range list.Data.Deployments {
		dseq, err := decodeSeq(dep.Deployment.ID.DSeq)
		if err != nil || dseq == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(dep.Deployment.State)) {
		case deployStateOpen, leaseStateActive:
		default:
			continue
		}
		url, err := d.controllerURLOf(ctx, dseq)
		if err != nil {
			// One unreadable deployment is not a reason to abandon the search; the
			// others may still hold the answer.
			d.Logf("akash: could not inspect dseq %s while looking for our own address: %v", dseq, err)
			continue
		}
		if url != "" {
			found = append(found, url)
		}
	}

	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		d.Logf("akash: %d deployments run the %s service (%s); not guessing which one is us",
			len(found), sdl.ControllerService, strings.Join(found, ", "))
		return "", nil
	}
}

// controllerURLOf reads one deployment and returns the controller service's address
// if it has an active lease running one.
func (d *Driver) controllerURLOf(ctx context.Context, dseq string) (string, error) {
	var out deploymentDetail
	if err := d.Client.do(ctx, "GET", "/v1/deployments/"+dseq, nil, &out); err != nil {
		if NotFound(err) {
			return "", nil
		}
		return "", err
	}
	for _, ld := range out.Data.Leases {
		if !strings.EqualFold(ld.State, leaseStateActive) {
			continue
		}
		if _, ok := ld.Status.Services[sdl.ControllerService]; !ok {
			continue
		}
		// requireIP is false: the controller is behind a shared endpoint by design,
		// and this is the same call the deploy made to learn the URL it printed.
		_, url, err := d.endpointFrom(ld.Status, sdl.ControllerService, false)
		if err != nil {
			// Leased but not yet routable. Not an error worth logging every tick.
			return "", nil
		}
		return url, nil
	}
	return "", nil
}
