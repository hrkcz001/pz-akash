// Package config defines the single source of truth for every tunable value in
// the PZ-on-Akash system. It is loaded from pz-saves/config.yaml.
//
// Two rules hold for this package and must keep holding:
//
//  1. No field in this schema may hold a secret. Secrets live in the process
//     environment and are loaded by internal/secrets. There is deliberately
//     nowhere in Config to put a password, which is what makes committing
//     config.yaml to a git repository safe.
//
//  2. An unknown key is a load error, not a silent default. A typo in
//     config.yaml fails startup instead of quietly reverting behaviour — the
//     failure mode that hid several bugs in the bash system.
//
// Comments name the environment variable each field replaces, so the migration
// is auditable.
package config

import "time"

// Config is the whole of pz-saves/config.yaml.
type Config struct {
	Version    int        `yaml:"version"`
	Identity   Identity   `yaml:"identity"`
	Git        Git        `yaml:"git"`
	Controller Controller `yaml:"controller"`
	Server     Server     `yaml:"server"`
	Akash      Akash      `yaml:"akash"`
	Backups    Backups    `yaml:"backups"`
	DNS        DNS        `yaml:"dns"`
	Game       Game       `yaml:"game"`
	Dashboard  Dashboard  `yaml:"dashboard"`
	Agent      Agent      `yaml:"agent"`
}

type Identity struct {
	// ServerName is the PZ server name; it also names the .ini and
	// _SandboxVars.lua files. Replaces SERVER_NAME.
	ServerName string `yaml:"server_name"`
	// Timezone is the single wall-clock reference for the whole system: backup
	// filenames, dashboard rendering, log lines, and bare `stop_at` timestamps
	// that carry no offset. An IANA name, so DST is handled for us.
	//
	// It is config rather than a property of the machine on purpose. v1 read the
	// clock with `date` inside the controller container, which has no TZ set and
	// no zoneinfo installed, so timestamps came out UTC by accident and would
	// change meaning if the image ever gained a tzdata package. Every timestamp
	// in v2 is either formatted through this location or stored with an explicit
	// offset; nothing depends on the host's own timezone.
	Timezone string `yaml:"timezone"`
}

type Git struct {
	RepoURL   string `yaml:"repo_url"`   // REPO_URL
	Branch    string `yaml:"branch"`     // human-owned config+triggers branch
	UserName  string `yaml:"user_name"`  // GIT_USER_NAME
	UserEmail string `yaml:"user_email"` // GIT_USER_EMAIL

	// Layout selects how runtime state is stored.
	//
	//	branches — state.json lives on ControllerStateBranch and agent.json on
	//	           AgentStateBranch, each force-pushed as a single orphan
	//	           commit. Enforces one writer per file structurally, keeps
	//	           Branch's history human-readable, and adds no history growth.
	//	single   — everything on Branch, as the bash system did.
	Layout                string `yaml:"layout"`
	ControllerStateBranch string `yaml:"controller_state_branch"`
	AgentStateBranch      string `yaml:"agent_state_branch"`

	// TriggersDir is the only directory whose changes fire the webhook. Making
	// it a directory is what lets us tell human intent apart from our own
	// state pushes without guessing.
	TriggersDir string `yaml:"triggers_dir"`

	// CacheDir is where the bare mirror of RepoURL lives. Bare, because no
	// process in v2 has a working tree: v1 kept one and then ran
	// `git reset --hard` on it, which is how a backup in progress lost its files
	// out from under itself.
	CacheDir string `yaml:"cache_dir"`

	// KnownHosts pins the SSH host keys of RepoURL, in known_hosts format
	// (multi-line). Host keys are public, so unlike the deploy key they belong
	// in config, and pinning them here is what lets a fresh container verify the
	// remote with no interactive first connection and no
	// StrictHostKeyChecking=no — which is what v1 used, and which accepts
	// whatever answers.
	KnownHosts string `yaml:"known_hosts"`

	// AllowUnverifiedHost disables host-key checking. It exists so a local test
	// against a file:// or localhost remote does not need pinned keys, and
	// validation refuses to let it be true at the same time as an ssh:// or
	// git@ RepoURL.
	AllowUnverifiedHost bool `yaml:"allow_unverified_host"`

	// MinPushInterval coalesces state pushes so a burst of transitions costs
	// one commit rather than one per transition.
	MinPushInterval Duration `yaml:"min_push_interval"`

	// NetTimeout bounds a single fetch or push. It is not a nicety: a git
	// operation blocked in a socket read cannot be cancelled, and the goroutine it
	// pins in the agent is the one that has to stop the game and save the world
	// when the lease closes. On expiry the operation is abandoned and retried on
	// the next tick.
	//
	// It must comfortably exceed a real transfer of the state branches (kilobytes)
	// but stay well under the container's SIGTERM grace period.
	NetTimeout Duration `yaml:"net_timeout"`
}

type Controller struct {
	Image    string `yaml:"image"`
	ImageTag string `yaml:"image_tag"`

	HTTPPort int `yaml:"http_port"` // HTTP_PORT
	// WebhookPort serves the GitHub webhook. 0 folds it onto HTTPPort and
	// drops the second exposed port from the SDL. Replaces WEBHOOK_PORT.
	WebhookPort int `yaml:"webhook_port"`

	Resources Resources `yaml:"resources"`
	// PricingAmount is the per-block bid ceiling in Akash.Price.Denom. The
	// controller is deployed by hand, so unlike the server's it is not computed.
	PricingAmount int            `yaml:"pricing_amount"`
	Poll          ControllerPoll `yaml:"poll"`
}

type ControllerPoll struct {
	// Tick is the FSM housekeeping interval: schedule checks, escrow, DNS
	// drift, lease health. It is not the trigger latency — triggers arrive by
	// webhook.
	Tick Duration `yaml:"tick"`
	// Idle is the git reconcile interval when no server is running.
	// Replaces WEBHOOK_POLL_SEC.
	Idle Duration `yaml:"idle"`
	// Active is the git reconcile interval while a server is running.
	// Replaces AUTOSAVER_POLL_SEC.
	Active Duration `yaml:"active"`
}

// Resources is an Akash compute profile. Sizes use Akash units (Ki/Mi/Gi/Ti);
// CPU is a string so millicores ("500m") stay expressible.
type Resources struct {
	CPU     string `yaml:"cpu"`
	Memory  string `yaml:"memory"`
	Storage string `yaml:"storage"`
}

type Server struct {
	Image    string `yaml:"image"`
	ImageTag string `yaml:"image_tag"`

	Resources Resources `yaml:"resources"`

	// MemoryMax/Min are JVM heap sizes injected into ProjectZomboid64.json,
	// e.g. "14336m". Replaces SERVER_MEMORY_MAX / SERVER_MEMORY_MIN.
	MemoryMax string `yaml:"memory_max"`
	MemoryMin string `yaml:"memory_min"`

	Ports ServerPorts `yaml:"ports"`

	// IPLease requests a dedicated Akash IP so the game ports keep their
	// numbers. Without it the provider assigns arbitrary external ports.
	IPLease bool `yaml:"ip_lease"`
	// IPName is the SDL `endpoints:` key referenced by each exposed port.
	IPName string `yaml:"ip_name"`

	// RCON is optional in v2: the agent owns the PZ process, so it can drive
	// saves through stdin and read player counts from stdout. Disabling it
	// closes the port entirely.
	RCON Feature `yaml:"rcon"`
	// SSH exists only for manual debugging now that backups are agent-side.
	SSH Feature `yaml:"ssh"`

	Crash Crash `yaml:"crash"`

	// OnlineTimeout bounds the wait for "*** SERVER STARTED ***" after a
	// lease goes ready. Replaces SERVER_ONLINE_TIMEOUT_SEC.
	OnlineTimeout Duration `yaml:"online_timeout"`

	// PricingAmount is only a placeholder for hand-deploys, in
	// Akash.Price.Denom; the real bid ceiling is computed from
	// Akash.Price.MaxUSDPerDay at deploy time.
	PricingAmount int `yaml:"pricing_amount"`
}

type ServerPorts struct {
	Game int `yaml:"game"` // PZ DefaultPort, udp
	UDP  int `yaml:"udp"`  // PZ UDPPort, udp
}

type Feature struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

type Crash struct {
	// MaxRestarts bounds in-process PZ relaunches while intent is "running".
	// The agent never exits, so this is a relaunch budget, not a pod budget.
	// Replaces MAX_CRASH_RESTARTS, which the bash version set and never read.
	MaxRestarts int      `yaml:"max_restarts"`
	Backoff     Duration `yaml:"backoff"`
}

type Akash struct {
	APIBase string `yaml:"api_base"` // API_BASE

	// DeployDays is the escrow horizon the funds loop tops up to, at the
	// actual lease price. Replaces DEPLOY_DAYS.
	DeployDays int `yaml:"deploy_days"`
	// InitialDepositDays is the deposit made at deploy time, at max price.
	// Replaces INITIAL_DEPOSIT_DAYS.
	InitialDepositDays int `yaml:"initial_deposit_days"`

	MaxAttempts  int `yaml:"max_attempts"`   // MAX_ATTEMPTS
	BlocksPerDay int `yaml:"blocks_per_day"` // BLOCKS_PER_DAY

	// AdoptUnleased lets adoption claim an open deployment that has no lease at
	// all. That is the wreckage of a controller that died between creating a
	// deployment and leasing it: escrow funded, nothing running, and no service
	// name to identify it by. Claiming it is how the deposit gets reclaimed rather
	// than stranded.
	//
	// Turn it off when the Akash wallet is shared with deployments this system did
	// not create, because a stranger's deployment is briefly unleased too, in the
	// seconds between its create and its lease.
	AdoptUnleased bool `yaml:"adopt_unleased"`

	Price     Price         `yaml:"price"`
	Placement Placement     `yaml:"placement"`
	Timeouts  AkashTimeouts `yaml:"timeouts"`
	Funds     Funds         `yaml:"funds"`
	API       AkashAPI      `yaml:"api"`

	ProviderStatus ProviderStatus `yaml:"provider_status"`
}

// AkashAPI configures the HTTP client that talks to Console: how hard it tries
// and how long any single request may take.
//
// These are here rather than in the code because they are the two numbers that
// decide whether a Console hiccup costs a retry or a deploy. v1 had neither — it
// was `curl` with no retry at all, which is how a single 502 became a failed cycle.
type AkashAPI struct {
	// Retries is how many extra attempts a retryable failure gets (429, 408, 5xx,
	// or a transport error). 3 means up to 4 requests.
	Retries int `yaml:"retries"`
	// RetryWait is the base backoff, doubled per attempt and capped at a minute.
	// A Retry-After header from the API overrides it.
	RetryWait Duration `yaml:"retry_wait"`
	// Timeout bounds one HTTP request, including its retries' individual attempts
	// but not the polling loops above them — those have their own deadlines in
	// akash.timeouts. It exists so a connection that opens and then stalls cannot
	// hold the controller past the point where the lease starts billing unwatched.
	Timeout Duration `yaml:"timeout"`
}

// ProviderStatus configures asking the provider directly for a lease's IP, as a
// fallback when the Console API's lease status has not caught up yet. Without it
// a deploy can time out waiting for an address the provider already has — which
// costs a redeploy and leaves the first lease's escrow to reclaim.
type ProviderStatus struct {
	Enabled bool `yaml:"enabled"`
	// Every is the lease-poll interval at which to try the provider: 6 means
	// every sixth poll. The Console API is the primary source and this is a
	// second opinion, not a replacement.
	Every int `yaml:"every"`
	// JWTTTL is the lifetime requested for the scoped status token.
	JWTTTL  Duration `yaml:"jwt_ttl"`
	Timeout Duration `yaml:"timeout"`
	// InsecureSkipVerify permits an unverified TLS connection to the provider —
	// what v1's `curl -sk` did unconditionally. Verification is always attempted
	// first and this is only the retry, because provider lease endpoints often
	// serve a certificate that does not match hostUri. The exchange is a
	// read-only status query carrying a token scoped to "status" and valid for
	// minutes, so the exposure is bounded; set false to require a valid chain
	// and accept losing the fallback.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

type Price struct {
	MaxUSDPerDay float64 `yaml:"max_usd_per_day"` // MAX_PRICE_USD
	MinUSDPerDay float64 `yaml:"min_usd_per_day"` // MIN_PRICE_USD
	// Tolerance widens the winning band: any bid within this fraction of the
	// cheapest is eligible, and the geographically closest of those wins.
	Tolerance float64 `yaml:"tolerance"` // PRICE_TOLERANCE
	// AKTUSDFallback is used only when the price oracle is unreachable. 0
	// means "no fallback, abort the cycle" — the old default. It is consulted
	// only when Denom is a denomination whose value depends on the AKT price.
	AKTUSDFallback float64 `yaml:"akt_usd_fallback"`
	PriceOracleURL string  `yaml:"price_oracle_url"`

	// Denom is the denomination our bid ceiling is expressed in, and it must be
	// the one the wallet spends: Console's managed wallets hold `uact`, a
	// dollar-pegged credit at 1e6 to the dollar, and price every bid in it.
	// `uakt` is micro-AKT and is the only denom that needs the oracle. Getting
	// this wrong does not overspend — it miscomputes the ceiling by the AKT
	// price and silently rejects every bid.
	Denom string `yaml:"denom"`

	// AllowedDenoms are the denominations a bid may be priced in. A bid in
	// anything else is skipped rather than guessed at, because a denom we cannot
	// convert is a price we cannot check.
	AllowedDenoms []string `yaml:"allowed_denoms"`
}

type Placement struct {
	// Countries is an explicit ISO 3166-1 alpha-2 allow list, replacing the
	// bash version's fuzzy country-name matching. Replaces EU_COUNTRY_CODES.
	Countries []string `yaml:"countries"`
	RefLat    float64  `yaml:"ref_lat"` // REF_LAT
	RefLon    float64  `yaml:"ref_lon"` // REF_LON
	// SkipTTL is how long a provider stays on the skip list after failing us.
	SkipTTL       Duration `yaml:"skip_ttl"` // SKIP_TTL_SEC
	DenyProviders []string `yaml:"deny_providers"`

	// MinUptime30d filters providers by the 30-day uptime the API reports, as a
	// fraction in [0, 1]. Replaces MIN_UPTIME30D. A provider that drops the lease
	// costs a redeploy and a world rolled back to the last backup, so this is the
	// cheapest filter we have; 0 disables it.
	MinUptime30d float64 `yaml:"min_uptime_30d"`
}

type AkashTimeouts struct {
	BidPoll       Duration `yaml:"bid_poll"`       // BID_POLL_SEC
	BidWait       Duration `yaml:"bid_wait"`       // BID_TIMEOUT_SEC
	LeasePoll     Duration `yaml:"lease_poll"`     // LEASE_POLL_SEC
	LeaseReady    Duration `yaml:"lease_ready"`    // LEASE_READY_TIMEOUT_SEC
	DepositSettle Duration `yaml:"deposit_settle"` // DEPOSIT_SETTLE_SEC
}

type Funds struct {
	CheckInterval Duration `yaml:"check_interval"` // FUNDS_CHECK_SEC
	MinTopupUSD   float64  `yaml:"min_topup_usd"`  // MIN_TOPUP_USD
	Margin        float64  `yaml:"margin"`         // DEPOSIT_MARGIN
}

type Backups struct {
	Dir string `yaml:"dir"` // BACKUP_DIR

	// Interval is the periodic backup cadence; 0 disables periodic backups.
	// Replaces BACKUP_INTERVAL_SEC.
	Interval Duration `yaml:"interval"`

	// RetentionDays and RetentionCount both apply; a backup is deleted when it
	// fails either. Count matters because Dir is ephemeral and finite.
	RetentionDays  int `yaml:"retention_days"` // BACKUP_RETENTION_DAYS
	RetentionCount int `yaml:"retention_count"`

	OnHalt bool `yaml:"on_halt"`
	// HaltTimeout bounds the wait for the agent's backup upload during a halt.
	HaltTimeout Duration `yaml:"halt_timeout"`
	// HaltConfirm bounds the wait for the agent to report PZ stopped.
	// Replaces HALT_CONFIRM_SEC.
	HaltConfirm Duration `yaml:"halt_confirm"`

	PauseFile string `yaml:"pause_file"`

	// DiskWarnPercent drives the dashboard warning that pushes the operator to
	// download backups before they are rotated or the lease is closed. This is
	// the only durability mechanism: Dir does not survive a redeploy.
	DiskWarnPercent int `yaml:"disk_warn_percent"`

	UploadMaxBytes int64 `yaml:"upload_max_bytes"`

	// RestorePolicy decides what the next boot restores, and it is spelled out in
	// config rather than implied by the code because the two reasonable answers
	// differ in which way they lose data. See RestoreLatest and friends.
	RestorePolicy string `yaml:"restore_policy"`
}

// Restore policies for Backups.RestorePolicy.
const (
	// RestoreLatest points the next boot at the newest completed backup — unless
	// an operator has pinned one, which always wins. The default, because with no
	// persistent storage the disk does not survive the lease: a boot that restores
	// nothing starts a fresh world over a perfectly good archive, so "start again"
	// has to mean "continue".
	RestoreLatest = "latest"

	// RestorePinned never follows a backup on its own; the target changes only
	// when an operator names one. Safer against the failure RestoreLatest cannot
	// see — a backup that faithfully captured a broken world — at the cost of
	// requiring a decision before every start.
	RestorePinned = "pinned"

	// RestoreNone does not restore at all: the target is held empty and a restore
	// trigger is refused rather than silently ignored. Every start is a fresh
	// world. For a deployment that wants a new map each session.
	RestoreNone = "none"
)

type DNS struct {
	Enabled  bool   `yaml:"enabled"`
	Provider string `yaml:"provider"`
	Domain   string `yaml:"domain"`  // CLOUDFLARE_DOMAIN
	ZoneID   string `yaml:"zone_id"` // CLOUDFLARE_ZONE_ID
	Proxied  bool   `yaml:"proxied"`
	SSLMode  string `yaml:"ssl_mode"`
	// IncludeWWW also upserts the www subdomain.
	IncludeWWW bool `yaml:"include_www"`

	// GameRecord is the subdomain label pointed at the game server's dedicated IP,
	// so players keep one address across redeploys instead of a fresh IP each time.
	// Empty disables it.
	//
	// This record is never proxied, whatever Proxied says, and that is not a
	// preference. Cloudflare's proxy carries HTTP only, and a proxied name resolves
	// to Cloudflare's addresses — so a player pasting a proxied name into the PZ
	// client would be sent to Cloudflare instead of the server. Proxying game
	// traffic needs Spectrum, which is a paid product this system does not use.
	GameRecord string `yaml:"game_record"`
}

// GameHost is the fully qualified name for the game server, or "" when no game
// record is configured.
func (d DNS) GameHost() string {
	if !d.Enabled || d.GameRecord == "" || d.Domain == "" {
		return ""
	}
	return d.GameRecord + "." + d.Domain
}

// Game holds the PZ server .ini values that the agent renders at boot. Secret
// .ini fields (RCONPassword, Password, AdminPassword) are deliberately absent:
// they arrive as placeholders in server.zip and are substituted by the
// controller when it serves that file.
type Game struct {
	Map        string `yaml:"map"`
	PublicName string `yaml:"public_name"`
	MaxPlayers int    `yaml:"max_players"`
	PauseEmpty bool   `yaml:"pause_empty"`
	Public     bool   `yaml:"public"`
	Open       bool   `yaml:"open"`
	// PasswordProtected maps to the ini Password= field and is independent of
	// Open: PZ enforces a join password whether or not accounts are open, which
	// is exactly how the v1 server ran (Open=true with a password set). When true
	// the join password is a required secret; when false the agent writes an
	// empty Password= so a hand-edited one cannot survive a config that says the
	// server is unprotected.
	PasswordProtected     bool `yaml:"password_protected"`
	GlobalChat            bool `yaml:"global_chat"`
	PingLimit             int  `yaml:"ping_limit"`
	MaxAccountsPerUser    int  `yaml:"max_accounts_per_user"`
	UPnP                  bool `yaml:"upnp"`
	SaveWorldEveryMinutes int  `yaml:"save_world_every_minutes"`

	// PZBackups are the game's own in-Saves backups, distinct from ours.
	PZBackups PZBackups `yaml:"pz_backups"`
}

type PZBackups struct {
	Count           int  `yaml:"count"`
	OnStart         bool `yaml:"on_start"`
	OnVersionChange bool `yaml:"on_version_change"`
	Period          int  `yaml:"period"`
}

type Dashboard struct {
	DefaultLocale string   `yaml:"default_locale"`
	Locales       []string `yaml:"locales"`
}

type Agent struct {
	// LivenessPush stamps agent.json even when nothing changed, so the
	// controller can distinguish "quiet" from "wedged".
	LivenessPush Duration `yaml:"liveness_push"`
	// PlayersPushMinInterval rate-limits player-count pushes. With git as the
	// bus, this is what keeps join/leave churn from becoming commit churn.
	PlayersPushMinInterval Duration `yaml:"players_push_min_interval"`

	// Reconcile is how often the agent re-reads the controller's branch for
	// intent, a restore target and a backup request. It bounds how long a halt
	// takes to be noticed, so it has to stay well under backups.halt_confirm.
	Reconcile Duration `yaml:"reconcile"`

	RestoreDownloadRetries int      `yaml:"restore_download_retries"`
	RestoreDownloadTimeout Duration `yaml:"restore_download_timeout"`

	Paths AgentPaths `yaml:"paths"`
	PZ    AgentPZ    `yaml:"pz"`
}

// AgentPaths is the container's filesystem layout.
//
// In v1 every one of these was a literal in entrypoint.sh or the Dockerfile, in
// several cases spelled out more than once, which meant the image and the script
// could disagree — and did, over the ~/Zomboid vs ~/zomboid case difference. They
// are configuration because they are properties of the image, and the image is
// something an operator replaces.
type AgentPaths struct {
	// Home is the account PZ runs under, and the root the rest default beneath.
	Home string `yaml:"home"`
	// GameDir is the PZ install, searched for a launch script.
	GameDir string `yaml:"game_dir"`
	// DataDir is what PZ calls ~/Zomboid: Server/, Saves/, db/, mods/.
	DataDir string `yaml:"data_dir"`
	// LowercaseLink is a symlink to DataDir under a lowercase name. The game
	// reads mods from ~/Zomboid but builds some internal paths in ~/zomboid
	// regardless of -cachedir, so on a case-sensitive filesystem both names have
	// to reach the same directory. Empty disables it.
	LowercaseLink string `yaml:"lowercase_link"`
	// RepoCache is the agent's own bare mirror of pz-saves. Separate from
	// git.cache_dir because that one belongs to the controller's container.
	RepoCache string `yaml:"repo_cache"`
	// WorkDir holds downloads and the zip being built. Anything here is
	// disposable and is cleaned on boot.
	WorkDir string `yaml:"work_dir"`
	// LogFile is where PZ's console output is mirrored, for an operator with a
	// shell. The agent reads the pipe, not this file: v1 read the file and had to
	// track a line offset across restarts to avoid re-matching an old banner.
	LogFile string `yaml:"log_file"`
}

// AgentPZ is how to drive the game process. These are PZ's interface, not ours,
// so they change when the game changes — which is exactly why they are config and
// not constants.
type AgentPZ struct {
	// LaunchScripts are candidate launcher names, tried in order.
	LaunchScripts []string `yaml:"launch_scripts"`
	// ReadyBanner is the line that means the server is accepting connections.
	ReadyBanner string `yaml:"ready_banner"`
	// ExtraArgs are appended to the launch command. -servername, -cachedir and
	// -adminpassword are supplied by the agent from config and secrets.
	ExtraArgs []string `yaml:"extra_args"`

	// SaveCommand and QuitCommand are written to PZ's stdin. This is why the
	// agent needs neither RCON nor SSH: it owns the process, so it owns the
	// console.
	SaveCommand string `yaml:"save_command"`
	QuitCommand string `yaml:"quit_command"`
	// SaveConfirm are substrings that mean the save finished, matched
	// case-insensitively against the console output. Several are listed because
	// the wording has changed between PZ builds, and an empty list means "wait
	// out SaveTimeout instead of watching" — a build that says something new
	// therefore costs a config line, not a rebuild.
	//
	// A confirmation the agent never sees is a warning, not a failure: v1 zipped
	// the world with no save at all, so proceeding is no worse than v1 while
	// waiting is strictly better.
	SaveConfirm []string `yaml:"save_confirm"`
	// SaveTimeout bounds the wait for a save to finish, QuitTimeout the wait for
	// the process to exit after being asked politely. After QuitTimeout the agent
	// escalates to a signal, then to a kill.
	SaveTimeout Duration `yaml:"save_timeout"`
	QuitTimeout Duration `yaml:"quit_timeout"`

	// PlayersCommand is polled to keep players_count live, which is bug 1. In v1
	// this was an RCON call the container never made, and eleven separate write
	// sites hardcoded a literal 0 instead.
	PlayersCommand  string   `yaml:"players_command"`
	PlayersInterval Duration `yaml:"players_interval"`
}

// WebhookOnHTTPPort reports whether the webhook shares the main HTTP port,
// which means the SDL exposes one port instead of two.
func (c *Config) WebhookOnHTTPPort() bool { return c.Controller.WebhookPort == 0 }

// EffectiveWebhookPort is the port the webhook actually listens on.
func (c *Config) EffectiveWebhookPort() int {
	if c.WebhookOnHTTPPort() {
		return c.Controller.HTTPPort
	}
	return c.Controller.WebhookPort
}

// ControllerImageRef is the fully qualified controller image.
func (c *Config) ControllerImageRef() string {
	return c.Controller.Image + ":" + c.Controller.ImageTag
}

// ServerImageRef is the fully qualified server image.
func (c *Config) ServerImageRef() string {
	return c.Server.Image + ":" + c.Server.ImageTag
}

// BranchLayout is the resolved set of refs the git bus reads and writes.
type BranchLayout struct {
	// Main is the operator-owned branch: config.yaml and the triggers directory.
	Main string
	// Controller and Agent are where each side publishes its document. In the
	// single layout both equal Main, which is the signal to write a fast-forward
	// child commit there rather than replace the branch.
	Controller  string
	Agent       string
	TriggersDir string
}

// BranchLayout resolves Git.Layout into concrete ref names.
// It exists so the layout switch is decided once, here, instead of at every
// place that opens the bus. The single layout leaves the two state branch fields
// empty in config — validation does not require them — and reading them
// unresolved would name the empty branch.
func (g Git) BranchLayout() BranchLayout {
	bl := BranchLayout{
		Main:        g.Branch,
		Controller:  g.ControllerStateBranch,
		Agent:       g.AgentStateBranch,
		TriggersDir: g.TriggersDir,
	}
	if g.Layout != LayoutBranches {
		bl.Controller, bl.Agent = g.Branch, g.Branch
	}
	return bl
}

// Location resolves Identity.Timezone. Validation has already rejected an
// unknown name, so this cannot fail in a running process; it falls back to UTC
// rather than panicking so a half-built Config in a test is still usable.
//
// Every wall-clock format in the system goes through here. Nothing calls
// time.Now().Format directly, which is what guarantees a backup made by the
// controller and one made by the agent carry the same timezone.
func (c *Config) Location() *time.Location {
	if c.Identity.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(c.Identity.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}
