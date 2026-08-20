package secrets

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const samplePEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA\n-----END OPENSSH PRIVATE KEY-----\n"

func TestDeployKeyPEMAcceptsTheFormsOperatorsActuallyProduce(t *testing.T) {
	std := base64.StdEncoding.EncodeToString([]byte(samplePEM))

	// A base64 value wrapped at 76 columns, which is what `base64` without -w0
	// emits and what a copy through a web form or a YAML block tends to preserve.
	var wrapped strings.Builder
	for i := 0; i < len(std); i += 40 {
		end := i + 40
		if end > len(std) {
			end = len(std)
		}
		wrapped.WriteString(std[i:end] + "\n")
	}

	for name, in := range map[string]string{
		"plain base64":       std,
		"wrapped base64":     wrapped.String(),
		"padded with spaces": "  " + std + "  ",
		"unpadded base64":    strings.TrimRight(std, "="),
		"raw PEM":            samplePEM,
	} {
		got, err := (&Set{DeployKeyB64: in}).DeployKeyPEM()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(string(got), "BEGIN OPENSSH PRIVATE KEY") {
			t.Errorf("%s: decoded to %q, which is not a private key", name, got)
		}
	}
}

func TestDeployKeyPEMEmptyIsNotAnError(t *testing.T) {
	// A local path remote needs no credential, and `state show` must run on a
	// machine that holds no secrets at all.
	for _, in := range []string{"", "   ", "\n"} {
		got, err := (&Set{DeployKeyB64: in}).DeployKeyPEM()
		if err != nil || got != nil {
			t.Errorf("DeployKeyPEM(%q) = %q, %v; want nil, nil", in, got, err)
		}
	}
}

func TestDeployKeyPEMRejectsGarbageByName(t *testing.T) {
	_, err := (&Set{DeployKeyB64: "not base64 !!! and not pem"}).DeployKeyPEM()
	if err == nil {
		t.Fatal("garbage was accepted as a key")
	}
	// The message has to name the variable. The alternative is diagnosing this
	// from a permission-denied at the far end of an SSH handshake.
	if !strings.Contains(err.Error(), DeployKeyEnv) {
		t.Fatalf("error %q does not name %s", err, DeployKeyEnv)
	}
}

func TestRequireDeployKeyNamesTheVariable(t *testing.T) {
	_, err := (&Set{}).RequireDeployKey()
	if !errors.Is(err, ErrNoDeployKey) {
		t.Fatalf("RequireDeployKey with nothing set = %v, want ErrNoDeployKey", err)
	}
	if !strings.Contains(err.Error(), DeployKeyEnv) {
		t.Fatalf("error %q does not name %s", err, DeployKeyEnv)
	}
	if _, err := (&Set{DeployKeyB64: samplePEM}).RequireDeployKey(); err != nil {
		t.Fatalf("RequireDeployKey with a key set: %v", err)
	}
}

// TestConditionalSecretsFollowTheirSwitch pins the three that are only sometimes
// required. Getting one of these wrong is not a missing feature: it refuses to
// start a controller over a value nothing would read.
func TestConditionalSecretsFollowTheirSwitch(t *testing.T) {
	// Everything unconditional, so the only variable is the conditional one.
	for _, n := range EnvNames() {
		switch n {
		case "PZ_JOIN_PASSWORD", "PZ_RCON_PASSWORD", "PZ_CLOUDFLARE_API_TOKEN":
		default:
			t.Setenv(n, "set")
		}
	}

	for _, tc := range []struct {
		what string
		req  Requirements
		env  string
	}{
		{"an unprotected server needs no join password", Requirements{}, ""},
		{"a protected server needs one", Requirements{JoinPassword: true}, "PZ_JOIN_PASSWORD"},
		{"rcon off needs no rcon password", Requirements{}, ""},
		{"rcon on needs one", Requirements{RCON: true}, "PZ_RCON_PASSWORD"},
		{"dns off needs no cloudflare token", Requirements{}, ""},
		{"dns on needs one", Requirements{DNS: true}, "PZ_CLOUDFLARE_API_TOKEN"},
	} {
		_, err := Load(RoleController, tc.req)
		switch {
		case tc.env == "" && err != nil:
			t.Errorf("%s: %v", tc.what, err)
		case tc.env != "" && err == nil:
			t.Errorf("%s: Load succeeded with %s unset", tc.what, tc.env)
		case tc.env != "" && !strings.Contains(err.Error(), tc.env):
			t.Errorf("%s: error %q does not name %s", tc.what, err, tc.env)
		}
	}
}

func TestLoadOptionalRequiresNothingButStillReadsEverything(t *testing.T) {
	t.Setenv(DeployKeyEnv, "  value  ")
	s := LoadOptional()
	if s.DeployKeyB64 != "value" {
		t.Errorf("DeployKeyB64 = %q, want the trimmed value", s.DeployKeyB64)
	}
	// Load would have failed here; LoadOptional must not.
	if _, err := Load(RoleController, Requirements{}); err == nil {
		t.Error("Load succeeded with only a deploy key set; the role requires more")
	}
}

// TestDeployKeyEnvIsInTheRegistry stops the constant and the registry drifting
// apart, which would make `pzctl config secrets` disagree with what the code reads.
func TestDeployKeyEnvIsInTheRegistry(t *testing.T) {
	for _, n := range EnvNames() {
		if n == DeployKeyEnv {
			return
		}
	}
	t.Fatalf("%s is not in EnvNames(): %v", DeployKeyEnv, EnvNames())
}
