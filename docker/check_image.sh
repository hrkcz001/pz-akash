#!/usr/bin/env bash
# check_image.sh ROLE IMAGE — assert what a built image must and must not contain.
#
# This is the "no secrets in layers" half of step 8's gate, plus the handful of
# facts that are true of a correct image and cheap to check from outside it. It
# runs in CI against an image loaded into the local daemon, and it is equally
# runnable by hand:
#
#   docker build -f docker/Dockerfile --target server -t pz-server .
#   docker/check_image.sh server pz-server
#
# Everything here is a property v1 got wrong at least once: passwords in the
# manifest and therefore in the image's environment, an sshd in the game image, a
# container running as root because gosu was doing the demotion, and a launcher
# script the config named but the image did not have.
set -euo pipefail

role="${1:?usage: check_image.sh ROLE IMAGE}"
image="${2:?usage: check_image.sh ROLE IMAGE}"

fails=0
fail() { echo "FAIL: $*" >&2; fails=$((fails + 1)); }
ok() { echo "ok: $*"; }

# inside runs a shell in the image as root, so a check can read a path the runtime
# user cannot. --entrypoint is empty in both images; this replaces CMD. (Not named
# `in`: that is a bash reserved word and the definition is a syntax error.)
inside() { docker run --rm --user 0:0 --entrypoint /bin/sh "$image" -c "$1"; }

inspect() { docker inspect --format "$1" "$image"; }

echo "== $role: $image"

# --- identity ---------------------------------------------------------------

want_user="pzctl"
want_cmd='[pzctl controller]'
if [ "$role" = server ]; then
  want_user="steam"
  want_cmd='[pzctl agent]'
fi

got_user="$(inspect '{{.Config.User}}')"
[ "$got_user" = "$want_user" ] &&
  ok "runs as $got_user" ||
  fail "Config.User is '$got_user', want '$want_user' (a root container is v1's arrangement)"

got_cmd="$(inspect '{{.Config.Cmd}}')"
[ "$got_cmd" = "$want_cmd" ] &&
  ok "CMD is $got_cmd" ||
  fail "Config.Cmd is '$got_cmd', want '$want_cmd'"

got_entry="$(inspect '{{.Config.Entrypoint}}')"
[ "$got_entry" = "[]" ] &&
  ok "no entrypoint script" ||
  fail "Config.Entrypoint is '$got_entry'; v2 has no entrypoint.sh"

inside 'test -x /usr/local/bin/pzctl' &&
  ok "pzctl is present and executable" ||
  fail "no executable /usr/local/bin/pzctl"

# --- no secrets -------------------------------------------------------------

# The image's own environment. Every secret reaches a container through the Akash
# manifest at deploy time; one baked here would be in the registry, readable by
# anyone who can pull.
secret_env="$(inspect '{{range .Config.Env}}{{println .}}{{end}}' |
  grep -Ei 'PASSWORD|SECRET|TOKEN|API_KEY|DEPLOY_KEY|PRIVATE' || true)"
if [ -n "$secret_env" ]; then
  fail "the image environment carries secret-shaped variables:"
  # Names only. Printing the line would put the value in a public build log.
  echo "$secret_env" | cut -d= -f1 | sed 's/^/    /' >&2
else
  ok "no secret-shaped environment variables"
fi

# Build arguments and RUN lines are public in the image history, which is why the
# build takes no secret arguments at all.
hist="$(docker history --no-trunc --format '{{.CreatedBy}}' "$image" |
  grep -Ei 'PASSWORD=|SECRET=|TOKEN=|API_KEY=|DEPLOY_KEY=' || true)"
[ -z "$hist" ] &&
  ok "no secret-shaped build arguments in the history" ||
  fail "image history mentions a secret-shaped variable"

# Credentials that end up in an image by accident: an ssh key from a build that
# cloned over ssh, a git credential store, a rendered v1 manifest.
found="$(inside 'find / -xdev \( -name "id_*" -o -name "*.pem" -o -name ".git-credentials" \
  -o -name "deployment.yaml" -o -path "*/.ssh/*" \) -type f 2>/dev/null | head -20' || true)"
[ -z "$found" ] &&
  ok "no key, credential or manifest files" ||
  { fail "credential-shaped files in the image:"; echo "$found" | sed 's/^/    /' >&2; }

# --- what the role needs, and what it must not have -------------------------

if [ "$role" = controller ]; then
  # The packages the dashboard serves. build_packages.py wrote the first three;
  # the Dockerfile copied the last two out of pz-saves.
  for f in packages_manifest.json common.zip server.zip; do
    inside "test -s /data/packages/$f" &&
      ok "/data/packages/$f" ||
      fail "/data/packages/$f is missing or empty"
  done
  for f in game.torrent README.md; do
    inside "test -s /data/packages/$f" &&
      ok "/data/packages/$f" ||
      echo "note: /data/packages/$f absent; the dashboard turns that section off"
  done

  # The directories the config names, writable by the runtime user. A controller
  # that cannot write its mirror fails at the first fetch, several minutes into a
  # deployment that already cost money.
  for d in /data /data/repo /data/backups; do
    inside "su pzctl -s /bin/sh -c 'test -w $d'" 2>/dev/null &&
      ok "$d is writable by pzctl" ||
      fail "$d is not writable by pzctl"
  done

  # The game passwords must reach the .ini as placeholders and be substituted as
  # the archive is served. Real values here would be a secret in a public layer.
  #
  # Pre-cutover this is a warning: pz-saves still holds the literals v1 committed,
  # and moving them is a step 9 item. It becomes a hard failure the moment that
  # lands — change the `note` below to `fail`.
  tmp="$(mktemp -d)"
  cid="$(docker create "$image")"
  docker cp "$cid:/data/packages/server.zip" "$tmp/server.zip" >/dev/null
  docker rm -f "$cid" >/dev/null
  unzip -o -q -d "$tmp/unz" "$tmp/server.zip" 'Server/*' || true

  literal=0
  missing=""
  for key in RCONPassword AdminPassword Password; do
    found=0
    while IFS= read -r line; do
      found=1
      value="${line#*=}"
      case "$value" in
      "" | __*__) : ;; # empty or a placeholder
      *)
        literal=$((literal + 1))
        # The key and the entry, never the value.
        echo "::warning::server.zip carries a literal $key" >&2
        ;;
      esac
    done < <(grep -rhE "^$key=" "$tmp/unz" 2>/dev/null || true)
    [ "$found" -eq 1 ] || missing="$missing $key"
  done
  [ "$literal" -eq 0 ] &&
    ok "server.zip has placeholders, not passwords" ||
    echo "note: $literal literal password(s) in server.zip — a step 9 (cutover) item"

  # An absent key is not a substituted key. The controller can only replace a token
  # that is there, so a missing AdminPassword line means PZ falls back to its own
  # default — and the placeholder machinery reports success while doing nothing.
  # This is the state pz-saves is in today: v1 passed the value as ADMIN_PASSWORD in
  # the server SDL's environment, so the committed ini never had the key at all.
  [ -z "$missing" ] ||
    echo "note:$missing absent from server.zip's ini — nothing for the controller to substitute"
  rm -rf "$tmp"
fi

if [ "$role" = server ]; then
  # The whole of the ssh path v1 needed for backups, and the demotion helper it
  # needed because it ran as root. Bug 4 was in that path; these absences are the
  # fix being real rather than intended.
  for bad in /usr/sbin/sshd /usr/sbin/gosu /usr/bin/gosu /usr/bin/sudo /run/sshd; do
    inside "test ! -e $bad" &&
      ok "no $bad" ||
      fail "$bad is present; v2 has no ssh or sudo path"
  done

  # A launcher the agent will actually find. agent.pz.launch_scripts lists these
  # two names and searches beneath game_dir in order, so either one satisfies it —
  # requiring a specific one would fail a perfectly good image. A config that names
  # a script the image does not have is a boot failure the agent can only report,
  # and this is the one place the two can be compared before a deployment.
  launcher=""
  for cand in start-server.sh StartServer64.sh; do
    if inside "test -x /home/steam/pz-server/$cand"; then
      launcher="$cand"
      break
    fi
  done
  [ -n "$launcher" ] &&
    ok "launcher $launcher is present and executable" ||
    fail "no launcher under /home/steam/pz-server; check agent.pz.launch_scripts"

  # PZ writes its world under $HOME. Docker does not read /etc/passwd for USER, so
  # an unset HOME would send the save to / and fail as a permission error minutes
  # into the first boot.
  got_home="$(inspect '{{range .Config.Env}}{{println .}}{{end}}' | grep '^HOME=' || true)"
  [ "$got_home" = "HOME=/home/steam" ] &&
    ok "HOME is /home/steam" ||
    fail "HOME is '$got_home', want HOME=/home/steam"
fi

echo
if [ "$fails" -ne 0 ]; then
  echo "$role: $fails check(s) failed" >&2
  exit 1
fi
echo "$role: all checks passed"
