// Package httpapi is the wire contract between the agent and the controller.
//
// It holds no logic — only the paths, header names and body shapes both sides
// have to agree on. The agent's client (internal/agent) and the controller's
// server (internal/httpapi, step 6) import the same constants, so a rename
// cannot land on one side only. In v1 the two sides were a bash `curl` line in
// entrypoint.sh and a hand-rolled `do_GET` router in storage_server.py, and the
// only thing keeping them in agreement was that nobody edited either.
//
// # Backup upload
//
// v1 uploaded a backup as a multipart POST to /upload, and the handler did
// `body_bytes = self.rfile.read(content_length)` — the whole archive into the
// controller's RAM, in a container with 2Gi. A 3Gi world would OOM-kill the
// controller mid-halt, which is the worst possible moment.
//
// v2 uses `PUT /backups/<name>` with the raw archive as the body:
//
//   - the agent streams from a file and the controller streams to one, so
//     neither holds the archive in memory,
//   - the name is in the path, so there is nothing to parse before deciding
//     where the bytes go,
//   - it is idempotent, so a retry after a half-finished upload is safe, and
//   - the digest travels in a header, so the receiver can verify while it
//     writes instead of trusting the sender.
package httpapi

import (
	"net/http"
	"strings"
)

// Paths served by the controller.
const (
	PathHealth = "/healthz"

	// PathCommonZip and PathClientZip are public: the client package is what
	// players download, and common.zip holds only mods and non-secret config.
	PathCommonZip = "/common.zip"
	PathClientZip = "/client.zip"

	// PathServerZip carries the .ini files with real passwords substituted in,
	// so it is guarded by RealmServerFiles.
	PathServerZip = "/server.zip"

	// PathBackupsDir is the prefix for one backup: GET to download, PUT to
	// upload. Guarded by RealmBackups.
	PathBackupsDir = "/backups/"

	// PathBackupsIndex is the machine-readable index of what is on the
	// controller's disk. The dashboard and the agent both read it.
	PathBackupsIndex = "/backups.json"
)

// Realm names the secret that guards a path. The zero value is deliberately
// "public" so a forgotten field opens nothing that was closed — a handler with
// no realm serves a public file, and every guarded path names its realm.
type Realm string

const (
	RealmPublic      Realm = ""
	RealmServerFiles Realm = "server-files"
	RealmBackups     Realm = "backups"
)

// Headers on a backup upload.
const (
	// HeaderSHA256 is the hex SHA-256 of the body. The receiver hashes as it
	// writes and rejects a mismatch, so a truncated upload cannot become a
	// backup that only fails when someone tries to restore from it.
	HeaderSHA256 = "X-PZ-SHA256"

	// HeaderRequestID echoes the controller's backup_request.id. It is what
	// makes an upload attributable: v1's controller could not tell the backup it
	// asked for from one that happened to arrive, which is bug 4.
	HeaderRequestID = "X-PZ-Request-Id"

	// HeaderPhase is the agent's phase at the moment of upload, for the
	// controller's log. Advisory only; nothing branches on it.
	HeaderPhase = "X-PZ-Phase"
)

// BackupPath is the URL path for one backup file.
func BackupPath(name string) string { return PathBackupsDir + name }

// BackupName extracts the file name from a /backups/<name> path. ok is false for
// anything else, including the bare directory and a nested path — the caller
// must not have to decide whether "/backups/../etc/passwd" counts.
func BackupName(urlPath string) (name string, ok bool) {
	if !strings.HasPrefix(urlPath, PathBackupsDir) {
		return "", false
	}
	name = strings.TrimPrefix(urlPath, PathBackupsDir)
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", false
	}
	return name, true
}

// UploadResult is the JSON body of a successful PUT. The agent compares the
// echoed digest with what it sent, so a controller that silently rewrote the
// file is caught by the uploader rather than by a future restore.
type UploadResult struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SetAuth attaches bearer credentials for a realm. Empty token is a no-op,
// which is what makes a public request and a guarded one the same code path.
func SetAuth(r *http.Request, token string) {
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
}

// BearerToken pulls the token out of an Authorization header. It returns "" for
// anything that is not exactly one Bearer credential, so a malformed header can
// never be mistaken for a matching empty password.
func BearerToken(h http.Header) string {
	v := h.Get("Authorization")
	const prefix = "Bearer "
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(v[len(prefix):])
}
