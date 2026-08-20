package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	// Embed the IANA timezone database so Identity.Timezone resolves
	// identically on a Windows workstation and in a scratch container.
	_ "time/tzdata"

	"github.com/hrkcz001/pz-akash/pzctl/internal/denom"
)

// ValidationError lists every problem found, not just the first. Fixing config
// one error per run is the kind of friction that leads to guessing.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d configuration problem(s):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

type problems struct{ list []string }

func (p *problems) addf(format string, args ...any) {
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

var (
	akashSizeRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti)$`)
	akashCPURe  = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?|[0-9]+m)$`)
	jvmSizeRe   = regexp.MustCompile(`^[0-9]+[kKmMgG]$`)
	countryRe   = regexp.MustCompile(`^[A-Z]{2}$`)
	nameRe      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	branchRe    = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// Validate checks the whole config and reports every problem at once.
func (c *Config) Validate() error {
	var p problems

	if c.Version != SchemaVersion {
		p.addf("version: got %d, this build understands %d", c.Version, SchemaVersion)
	}

	c.validateIdentity(&p)
	c.validateGit(&p)
	c.validateController(&p)
	c.validateServer(&p)
	c.validateAkash(&p)
	c.validateBackups(&p)
	c.validateDNS(&p)
	c.validateGame(&p)
	c.validateDashboard(&p)
	c.validateAgent(&p)

	if len(p.list) > 0 {
		return &ValidationError{Problems: p.list}
	}
	return nil
}

func (c *Config) validateIdentity(p *problems) {
	switch {
	case c.Identity.ServerName == "":
		p.addf("identity.server_name: required (it names the PZ .ini and SandboxVars files)")
	case !nameRe.MatchString(c.Identity.ServerName):
		p.addf("identity.server_name: %q must match %s — it is used as a filename",
			c.Identity.ServerName, nameRe)
	}
	if c.Identity.Timezone == "" {
		p.addf("identity.timezone: required (use UTC if unsure)")
	} else if _, err := time.LoadLocation(c.Identity.Timezone); err != nil {
		p.addf("identity.timezone: %q is not a known IANA timezone", c.Identity.Timezone)
	}
}

func (c *Config) validateGit(p *problems) {
	g := c.Git
	if g.RepoURL == "" {
		p.addf("git.repo_url: required")
	}
	requireBranch(p, "git.branch", g.Branch)

	switch g.Layout {
	case LayoutBranches:
		requireBranch(p, "git.controller_state_branch", g.ControllerStateBranch)
		requireBranch(p, "git.agent_state_branch", g.AgentStateBranch)
		// The whole point of the branches layout is that one writer cannot
		// reach another's file. Collapsing two of these onto one branch would
		// silently undo that.
		if g.ControllerStateBranch == g.AgentStateBranch {
			p.addf("git.controller_state_branch and git.agent_state_branch must differ (single-writer ownership depends on it)")
		}
		if g.ControllerStateBranch == g.Branch || g.AgentStateBranch == g.Branch {
			p.addf("git.{controller,agent}_state_branch must differ from git.branch")
		}
	case LayoutSingle:
		// Nothing further: all files share one branch by design.
	default:
		p.addf("git.layout: %q must be %q or %q", g.Layout, LayoutBranches, LayoutSingle)
	}

	switch {
	case g.TriggersDir == "":
		p.addf("git.triggers_dir: required (webhook filtering keys off it)")
	case strings.HasPrefix(g.TriggersDir, "/"), strings.HasSuffix(g.TriggersDir, "/"):
		p.addf("git.triggers_dir: %q must not begin or end with %q", g.TriggersDir, "/")
	case strings.Contains(g.TriggersDir, ".."):
		p.addf("git.triggers_dir: %q must not contain %q", g.TriggersDir, "..")
	}

	if g.MinPushInterval < 0 {
		p.addf("git.min_push_interval: must not be negative")
	}

	// Zero would mean "no bound", and an unbounded git operation is the one that
	// pins the agent's loop past the container's grace period.
	if g.NetTimeout <= 0 {
		p.addf("git.net_timeout: required and must be positive (a git operation blocked in a socket read cannot be cancelled)")
	}

	if g.CacheDir == "" {
		p.addf("git.cache_dir: required (the bare mirror has to live somewhere)")
	} else if !isAbsPath(g.CacheDir) {
		// Relative would resolve against whatever directory the process happened
		// to start in, which differs between the container and an operator's shell.
		p.addf("git.cache_dir: %q must be an absolute path", g.CacheDir)
	}

	// Host-key verification is not optional over SSH. Leaving it off was v1's
	// posture (StrictHostKeyChecking=no), which accepts any host that answers on
	// port 22 — and the credential it then presents is a repository write key.
	sshRemote := strings.HasPrefix(g.RepoURL, "git@") ||
		strings.HasPrefix(g.RepoURL, "ssh://")
	switch {
	case sshRemote && g.AllowUnverifiedHost:
		p.addf("git.allow_unverified_host: must be false for the SSH remote %q; pin git.known_hosts instead", g.RepoURL)
	case sshRemote && g.KnownHosts == "":
		p.addf("git.known_hosts: required for the SSH remote %q (paste `ssh-keyscan github.com`; host keys are public, so they belong in config)", g.RepoURL)
	}
}

// isAbsPath accepts both container paths (/data/repo) and Windows paths
// (C:\tmp\repo), because config.yaml is validated on an operator's machine as
// well as inside the image, and filepath.IsAbs would answer differently there.
func isAbsPath(s string) bool {
	if strings.HasPrefix(s, "/") {
		return true
	}
	return len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/')
}

func (c *Config) validateController(p *problems) {
	ct := c.Controller
	if ct.Image == "" {
		p.addf("controller.image: required")
	}
	if ct.ImageTag == "" {
		p.addf("controller.image_tag: required")
	}
	requirePort(p, "controller.http_port", ct.HTTPPort)
	if ct.WebhookPort != 0 {
		requirePort(p, "controller.webhook_port", ct.WebhookPort)
		if ct.WebhookPort == ct.HTTPPort {
			p.addf("controller.webhook_port: equals http_port (%d); use 0 to serve the webhook on http_port instead", ct.HTTPPort)
		}
	}
	validateResources(p, "controller.resources", ct.Resources)
	requirePositive(p, "controller.pricing_amount", ct.PricingAmount)
	requirePositiveDur(p, "controller.poll.tick", ct.Poll.Tick)
	requirePositiveDur(p, "controller.poll.idle", ct.Poll.Idle)
	requirePositiveDur(p, "controller.poll.active", ct.Poll.Active)
}

func (c *Config) validateServer(p *problems) {
	s := c.Server
	if s.Image == "" {
		p.addf("server.image: required")
	}
	if s.ImageTag == "" {
		p.addf("server.image_tag: required")
	}
	validateResources(p, "server.resources", s.Resources)

	for _, f := range []struct {
		path, val string
	}{
		{"server.memory_max", s.MemoryMax},
		{"server.memory_min", s.MemoryMin},
	} {
		if !jvmSizeRe.MatchString(f.val) {
			p.addf("%s: %q must be a JVM heap size such as 14336m or 14g", f.path, f.val)
		}
	}

	// Collect every port that will appear in the SDL and reject duplicates —
	// two `expose` entries on one port is rejected by Akash at deploy time,
	// which is a slow and confusing way to learn about a typo.
	used := map[int]string{}
	claim := func(path string, port int) {
		requirePort(p, path, port)
		if port < 1 || port > 65535 {
			return
		}
		if prev, dup := used[port]; dup {
			p.addf("%s: port %d is already used by %s", path, port, prev)
			return
		}
		used[port] = path
	}
	claim("server.ports.game", s.Ports.Game)
	claim("server.ports.udp", s.Ports.UDP)
	if s.RCON.Enabled {
		claim("server.rcon.port", s.RCON.Port)
	}
	if s.SSH.Enabled {
		claim("server.ssh.port", s.SSH.Port)
	}

	if s.IPLease {
		if s.IPName == "" {
			p.addf("server.ip_name: required when server.ip_lease is true (it is the SDL endpoints: key)")
		} else if !nameRe.MatchString(s.IPName) {
			p.addf("server.ip_name: %q must match %s", s.IPName, nameRe)
		}
	}

	if s.Crash.MaxRestarts < 0 {
		p.addf("server.crash.max_restarts: must not be negative")
	}
	if s.Crash.Backoff < 0 {
		p.addf("server.crash.backoff: must not be negative")
	}
	requirePositiveDur(p, "server.online_timeout", s.OnlineTimeout)
	requirePositive(p, "server.pricing_amount", s.PricingAmount)
}

func (c *Config) validateAkash(p *problems) {
	a := c.Akash
	requireHTTPURL(p, "akash.api_base", a.APIBase)
	// The oracle URL is optional, and only checked as a URL when it is set: a
	// uact wallet never asks for a rate, and an AKT one may carry a fallback
	// instead. Requiring it unconditionally would reject a config the code below
	// (and Oracle.Rate) both accept.
	if a.Price.PriceOracleURL != "" {
		requireHTTPURL(p, "akash.price.price_oracle_url", a.Price.PriceOracleURL)
	}

	requirePositive(p, "akash.deploy_days", a.DeployDays)
	requirePositive(p, "akash.initial_deposit_days", a.InitialDepositDays)
	if a.InitialDepositDays > a.DeployDays {
		p.addf("akash.initial_deposit_days (%d) exceeds akash.deploy_days (%d)", a.InitialDepositDays, a.DeployDays)
	}
	requirePositive(p, "akash.max_attempts", a.MaxAttempts)
	requirePositive(p, "akash.blocks_per_day", a.BlocksPerDay)

	if a.Price.MinUSDPerDay < 0 {
		p.addf("akash.price.min_usd_per_day: must not be negative")
	}
	if a.Price.MaxUSDPerDay <= 0 {
		p.addf("akash.price.max_usd_per_day: must be greater than 0")
	}
	if a.Price.MaxUSDPerDay <= a.Price.MinUSDPerDay {
		p.addf("akash.price.max_usd_per_day (%g) must exceed min_usd_per_day (%g)", a.Price.MaxUSDPerDay, a.Price.MinUSDPerDay)
	}
	if a.Price.Tolerance < 0 || a.Price.Tolerance > 1 {
		p.addf("akash.price.tolerance: %g must be between 0 and 1", a.Price.Tolerance)
	}
	if a.Price.AKTUSDFallback < 0 {
		p.addf("akash.price.akt_usd_fallback: must not be negative (0 means no fallback)")
	}
	if len(a.Price.AllowedDenoms) == 0 {
		p.addf("akash.price.allowed_denoms: at least one denomination is required")
	}
	for i, d := range a.Price.AllowedDenoms {
		if !denom.Known(d) {
			p.addf("akash.price.allowed_denoms[%d]: %q is not a denomination this build can convert to USD (known: %s, %s)", i, d, denom.UACT, denom.UAKT)
		}
	}
	if !denom.Known(a.Price.Denom) {
		p.addf("akash.price.denom: %q is not a denomination this build can convert to USD (known: %s, %s)", a.Price.Denom, denom.UACT, denom.UAKT)
	} else if !slices.Contains(a.Price.AllowedDenoms, a.Price.Denom) {
		// Bidding in a denomination we would then refuse to read back is a deploy
		// that always ends in "no eligible bids".
		p.addf("akash.price.denom: %q must also appear in allowed_denoms %v", a.Price.Denom, a.Price.AllowedDenoms)
	}
	// The oracle is only on the critical path for AKT-denominated bids. Saying so
	// here is what lets a uact deployment start with CoinGecko unreachable.
	if denom.NeedsOracle(a.Price.Denom) && a.Price.AKTUSDFallback == 0 && a.Price.PriceOracleURL == "" {
		p.addf("akash.price: denom %s needs either price_oracle_url or akt_usd_fallback", a.Price.Denom)
	}
	// A hand-deploy placeholder above the stated dollar limit is a ceiling that
	// contradicts the config it sits next to. Only checkable when the denomination
	// does not need a live rate.
	if denom.Known(a.Price.Denom) && !denom.NeedsOracle(a.Price.Denom) && a.Price.MaxUSDPerDay > 0 {
		for _, f := range []struct {
			key    string
			amount int
		}{
			{"controller.pricing_amount", c.Controller.PricingAmount},
			{"server.pricing_amount", c.Server.PricingAmount},
		} {
			if f.amount <= 0 {
				continue // reported by the per-section validators
			}
			usd, err := denom.USDPerDay(float64(f.amount), a.Price.Denom, a.BlocksPerDay, 0)
			if err != nil || usd <= a.Price.MaxUSDPerDay {
				continue
			}
			p.addf("%s: %d %s/block is $%.2f/day, above akash.price.max_usd_per_day ($%.2f)",
				f.key, f.amount, a.Price.Denom, usd, a.Price.MaxUSDPerDay)
		}
	}

	if len(a.Placement.Countries) == 0 {
		p.addf("akash.placement.countries: at least one ISO 3166-1 alpha-2 code is required")
	}
	for i, code := range a.Placement.Countries {
		if !countryRe.MatchString(code) {
			p.addf("akash.placement.countries[%d]: %q must be a two-letter uppercase ISO 3166-1 code", i, code)
		}
	}
	if a.Placement.RefLat < -90 || a.Placement.RefLat > 90 {
		p.addf("akash.placement.ref_lat: %g is out of range [-90, 90]", a.Placement.RefLat)
	}
	if a.Placement.RefLon < -180 || a.Placement.RefLon > 180 {
		p.addf("akash.placement.ref_lon: %g is out of range [-180, 180]", a.Placement.RefLon)
	}
	requirePositiveDur(p, "akash.placement.skip_ttl", a.Placement.SkipTTL)
	if a.Placement.MinUptime30d < 0 || a.Placement.MinUptime30d > 1 {
		p.addf("akash.placement.min_uptime_30d: %g must be a fraction between 0 and 1", a.Placement.MinUptime30d)
	}

	requirePositiveDur(p, "akash.timeouts.bid_poll", a.Timeouts.BidPoll)
	requirePositiveDur(p, "akash.timeouts.bid_wait", a.Timeouts.BidWait)
	requirePositiveDur(p, "akash.timeouts.lease_poll", a.Timeouts.LeasePoll)
	requirePositiveDur(p, "akash.timeouts.lease_ready", a.Timeouts.LeaseReady)
	requirePositiveDur(p, "akash.timeouts.deposit_settle", a.Timeouts.DepositSettle)

	requirePositiveDur(p, "akash.funds.check_interval", a.Funds.CheckInterval)
	if a.Funds.MinTopupUSD <= 0 {
		p.addf("akash.funds.min_topup_usd: must be greater than 0")
	}
	if a.Funds.Margin < 1 {
		p.addf("akash.funds.margin: %g must be at least 1 (it is a multiplier over the computed cost)", a.Funds.Margin)
	}

	// 0 retries is a legitimate choice (fail fast, let the FSM's attempt loop
	// handle it); a negative count is a typo that would read as "no attempts".
	if a.API.Retries < 0 {
		p.addf("akash.api.retries: must not be negative (0 means one attempt with no retry)")
	}
	if a.API.RetryWait <= 0 {
		p.addf("akash.api.retry_wait: must be greater than 0")
	}
	requirePositiveDur(p, "akash.api.timeout", a.API.Timeout)
	// A request timeout shorter than the poll interval it is used from would
	// cancel every poll on the way out. This is the pair that would actually bite.
	if a.API.Timeout > 0 && a.Timeouts.BidWait > 0 && a.API.Timeout.D() > a.Timeouts.BidWait.D() {
		p.addf("akash.api.timeout (%v) exceeds akash.timeouts.bid_wait (%v) — one stalled request would consume the whole bid window",
			a.API.Timeout.D(), a.Timeouts.BidWait.D())
	}
}

func (c *Config) validateBackups(p *problems) {
	b := c.Backups
	if b.Dir == "" {
		p.addf("backups.dir: required")
	}
	if b.Interval < 0 {
		p.addf("backups.interval: must not be negative (0 disables periodic backups)")
	}
	if b.RetentionDays < 0 {
		p.addf("backups.retention_days: must not be negative")
	}
	// backups.dir is ephemeral by design, so an unbounded count is a way to
	// fill the disk and lose the server, not a way to keep more history.
	if b.RetentionCount < 1 {
		p.addf("backups.retention_count: must be at least 1 (backups.dir is ephemeral and finite)")
	}
	requirePositiveDur(p, "backups.halt_timeout", b.HaltTimeout)
	requirePositiveDur(p, "backups.halt_confirm", b.HaltConfirm)
	if b.PauseFile == "" {
		p.addf("backups.pause_file: required")
	}
	if b.DiskWarnPercent < 1 || b.DiskWarnPercent > 100 {
		p.addf("backups.disk_warn_percent: %d must be between 1 and 100", b.DiskWarnPercent)
	}
	if b.UploadMaxBytes <= 0 {
		p.addf("backups.upload_max_bytes: must be greater than 0")
	}
	switch b.RestorePolicy {
	case RestoreLatest, RestorePinned, RestoreNone:
	default:
		// Not defaulted silently: an unrecognised value here decides whether a start
		// continues the world or replaces it, and guessing on the operator's behalf
		// is exactly the silence this key exists to remove.
		p.addf("backups.restore_policy: %q must be %q, %q or %q",
			b.RestorePolicy, RestoreLatest, RestorePinned, RestoreNone)
	}
}

func (c *Config) validateDNS(p *problems) {
	if !c.DNS.Enabled {
		return
	}
	d := c.DNS
	if d.Provider != "cloudflare" {
		p.addf("dns.provider: %q is not supported (only \"cloudflare\")", d.Provider)
	}
	if d.Domain == "" {
		p.addf("dns.domain: required when dns.enabled is true")
	}
	if d.ZoneID == "" {
		p.addf("dns.zone_id: required when dns.enabled is true")
	}
	requireHTTPURL(p, "dns.api_base", d.APIBase)
	requirePositiveDur(p, "dns.timeout", d.Timeout)
	if d.Retries < 0 {
		p.addf("dns.retries: must not be negative (0 means one attempt with no retry)")
	}
	if d.RetryWait <= 0 {
		p.addf("dns.retry_wait: must be greater than 0")
	}
	// 1 is Cloudflare's "automatic", and anything between 2 and 59 is rejected by
	// the API — a 400 at deploy time, on a lease that is already billing.
	if d.GameRecord != "" && d.GameTTL != 1 && (d.GameTTL < 60 || d.GameTTL > 86400) {
		p.addf("dns.game_ttl: %d must be 1 (automatic) or between 60 and 86400 seconds", d.GameTTL)
	}
	switch d.SSLMode {
	case "off", "flexible", "full", "strict":
	default:
		p.addf("dns.ssl_mode: %q must be one of off, flexible, full, strict", d.SSLMode)
	}
	// A malformed label is worth catching here rather than in a Cloudflare 400 at
	// deploy time: by then the lease exists and is billing.
	if d.GameRecord != "" {
		if err := checkDNSName(d.GameRecord); err != nil {
			p.addf("dns.game_record: %v", err)
		}
		if d.GameRecord == "www" && d.IncludeWWW {
			p.addf("dns.game_record: %q collides with dns.include_www, which points the same name at the controller", d.GameRecord)
		}
	}
}

// checkDNSName validates a subdomain, which may be several labels deep.
func checkDNSName(s string) error {
	if len(s) > 253 {
		return fmt.Errorf("%q is longer than 253 characters", s)
	}
	for _, label := range strings.Split(s, ".") {
		switch {
		case label == "":
			return fmt.Errorf("%q has an empty label", s)
		case len(label) > 63:
			return fmt.Errorf("label %q is longer than 63 characters", label)
		case strings.HasPrefix(label, "-"), strings.HasSuffix(label, "-"):
			return fmt.Errorf("label %q must not start or end with a hyphen", label)
		}
		for _, r := range label {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-'
			if !ok {
				return fmt.Errorf("label %q contains %q; only letters, digits and hyphens are allowed", label, r)
			}
		}
	}
	return nil
}

func (c *Config) validateGame(p *problems) {
	g := c.Game
	if g.Map == "" {
		p.addf("game.map: required")
	}
	requirePositive(p, "game.max_players", g.MaxPlayers)
	if g.PingLimit < 0 {
		p.addf("game.ping_limit: must not be negative")
	}
	if g.MaxAccountsPerUser < 0 {
		p.addf("game.max_accounts_per_user: must not be negative (0 means unlimited)")
	}
	if g.SaveWorldEveryMinutes < 0 {
		p.addf("game.save_world_every_minutes: must not be negative")
	}
	if g.PZBackups.Count < 0 {
		p.addf("game.pz_backups.count: must not be negative")
	}
	if g.PZBackups.Period < 0 {
		p.addf("game.pz_backups.period: must not be negative")
	}
}

func (c *Config) validateDashboard(p *problems) {
	d := c.Dashboard
	if len(d.Locales) == 0 {
		p.addf("dashboard.locales: at least one locale is required")
	}
	if d.DefaultLocale == "" {
		p.addf("dashboard.default_locale: required")
		return
	}
	for _, l := range d.Locales {
		if l == d.DefaultLocale {
			return
		}
	}
	p.addf("dashboard.default_locale: %q is not listed in dashboard.locales (%s)",
		d.DefaultLocale, strings.Join(d.Locales, ", "))
}

func (c *Config) validateAgent(p *problems) {
	a := c.Agent
	requirePositiveDur(p, "agent.liveness_push", a.LivenessPush)
	if a.PlayersPushMinInterval < 0 {
		p.addf("agent.players_push_min_interval: must not be negative")
	}
	requirePositiveDur(p, "agent.reconcile", a.Reconcile)
	// A halt is only noticed on a reconcile, and the controller stops waiting
	// after halt_confirm. Polling more slowly than that turns every halt into a
	// timeout, which is one of the ways v1 ended up force-closing leases.
	if a.Reconcile > 0 && c.Backups.HaltConfirm > 0 && a.Reconcile.D() >= c.Backups.HaltConfirm.D() {
		p.addf("agent.reconcile (%v) must be shorter than backups.halt_confirm (%v) — a halt is only seen on a reconcile",
			a.Reconcile.D(), c.Backups.HaltConfirm.D())
	}
	requirePositive(p, "agent.restore_download_retries", a.RestoreDownloadRetries)
	requirePositiveDur(p, "agent.restore_download_timeout", a.RestoreDownloadTimeout)
	c.validateAgentPaths(p)
	c.validateAgentPZ(p)
}

func (c *Config) validateAgentPaths(p *problems) {
	a := c.Agent.Paths
	requireAbsPath(p, "agent.paths.home", a.Home)
	requireAbsPath(p, "agent.paths.game_dir", a.GameDir)
	requireAbsPath(p, "agent.paths.data_dir", a.DataDir)
	requireAbsPath(p, "agent.paths.repo_cache", a.RepoCache)
	requireAbsPath(p, "agent.paths.work_dir", a.WorkDir)
	requireAbsPath(p, "agent.paths.log_file", a.LogFile)
	// Empty is how the symlink is disabled, so it is only checked when set.
	if a.LowercaseLink != "" {
		requireAbsPath(p, "agent.paths.lowercase_link", a.LowercaseLink)
		if a.LowercaseLink == a.DataDir {
			// The agent creates the link pointing at data_dir. Equal paths would
			// make it either a self-link or a delete of the save directory,
			// depending on which check ran first.
			p.addf("agent.paths.lowercase_link: must differ from agent.paths.data_dir (%s)", a.DataDir)
		}
	}
	// work_dir is emptied on boot. Naming a directory that holds anything else is
	// therefore a data-loss bug, and these are the two that would hurt.
	for _, other := range []struct{ key, val string }{
		{"agent.paths.data_dir", a.DataDir},
		{"agent.paths.game_dir", a.GameDir},
	} {
		if a.WorkDir != "" && a.WorkDir == other.val {
			p.addf("agent.paths.work_dir: must differ from %s — it is emptied on boot", other.key)
		}
	}
}

func (c *Config) validateAgentPZ(p *problems) {
	z := c.Agent.PZ
	if len(z.LaunchScripts) == 0 {
		p.addf("agent.pz.launch_scripts: at least one launcher name is required")
	}
	for i, s := range z.LaunchScripts {
		switch {
		case s == "":
			p.addf("agent.pz.launch_scripts[%d]: must not be empty", i)
		case strings.ContainsAny(s, `/\`):
			// A bare name, because it is searched for beneath game_dir. A path
			// here would silently never match.
			p.addf("agent.pz.launch_scripts[%d]: %q must be a file name, not a path", i, s)
		}
	}
	if z.ReadyBanner == "" {
		// Without it the agent has no way to tell booting from online, and would
		// have to call the server ready the moment the process started.
		p.addf("agent.pz.ready_banner: required — it is how online is detected")
	}
	if z.SaveCommand == "" {
		p.addf("agent.pz.save_command: required — a backup with no prior save is a torn world")
	}
	if z.QuitCommand == "" {
		p.addf("agent.pz.quit_command: required — without it a halt can only kill the process")
	}
	requirePositiveDur(p, "agent.pz.save_timeout", z.SaveTimeout)
	requirePositiveDur(p, "agent.pz.quit_timeout", z.QuitTimeout)
	if z.PlayersCommand == "" {
		p.addf("agent.pz.players_command: required — it is what keeps players_count from being stuck at 0")
	}
	requirePositiveDur(p, "agent.pz.players_interval", z.PlayersInterval)
}

// --- shared checks ---

func validateResources(p *problems, path string, r Resources) {
	if !akashCPURe.MatchString(r.CPU) {
		p.addf("%s.cpu: %q must be a number of vCPU (\"8\", \"0.5\") or millicores (\"500m\")", path, r.CPU)
	}
	for _, f := range []struct{ key, val string }{{"memory", r.Memory}, {"storage", r.Storage}} {
		if !akashSizeRe.MatchString(f.val) {
			p.addf("%s.%s: %q must be an Akash size such as 512Mi or 16Gi", path, f.key, f.val)
		}
	}
}

func requirePort(p *problems, path string, port int) {
	if port < 1 || port > 65535 {
		p.addf("%s: %d is not a valid TCP/UDP port", path, port)
	}
}

func requirePositive(p *problems, path string, v int) {
	if v <= 0 {
		p.addf("%s: must be greater than 0", path)
	}
}

// requireAbsPath accepts both a POSIX absolute path and a Windows one.
//
// filepath.IsAbs alone would be wrong here: these are paths inside a Linux
// container, and `config validate` is run on a Windows workstation and in CI,
// where filepath.IsAbs("/home/steam") is false. Accepting both means the same
// committed config validates everywhere, and a local agent run against a scratch
// directory (the step 4 gate) can still point these at C:\...
func requireAbsPath(p *problems, path, v string) {
	switch {
	case v == "":
		p.addf("%s: required", path)
	case strings.HasPrefix(v, "/"), filepath.IsAbs(v):
	default:
		p.addf("%s: %q must be an absolute path", path, v)
	}
}

func requirePositiveDur(p *problems, path string, d Duration) {
	if d <= 0 {
		p.addf("%s: must be greater than 0", path)
	}
}

func requireBranch(p *problems, path, name string) {
	switch {
	case name == "":
		p.addf("%s: required", path)
	case !branchRe.MatchString(name):
		p.addf("%s: %q is not a valid git branch name", path, name)
	}
}

func requireHTTPURL(p *problems, path, raw string) {
	if raw == "" {
		p.addf("%s: required", path)
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		p.addf("%s: %q is not a valid URL: %v", path, raw, err)
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		p.addf("%s: %q must use http or https", path, raw)
	}
	if u.Host == "" {
		p.addf("%s: %q has no host", path, raw)
	}
}
