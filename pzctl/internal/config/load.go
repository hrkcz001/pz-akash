package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultFileName is the config file name inside the pz-saves repository.
const DefaultFileName = "config.yaml"

// SchemaVersion is the config schema this build understands.
const SchemaVersion = 1

// Defaults returns a Config prefilled with every default. Loading decodes the
// YAML on top of this, so an omitted key keeps its default and a present key
// overrides it. Fields with no sensible default (repo URL, image names, domain)
// are left zero and rejected by Validate.
func Defaults() *Config {
	return &Config{
		Version: SchemaVersion,
		Identity: Identity{
			Timezone: "UTC",
		},
		Git: Git{
			Branch:                "main",
			Layout:                LayoutBranches,
			ControllerStateBranch: "state/controller",
			AgentStateBranch:      "state/agent",
			TriggersDir:           "triggers",
			CacheDir:              "/data/repo",
			MinPushInterval:       Duration(5 * time.Second),
			NetTimeout:            Duration(45 * time.Second),
		},
		Controller: Controller{
			HTTPPort:    8000,
			WebhookPort: 8080,
			Resources:   Resources{CPU: "1", Memory: "2Gi", Storage: "20Gi"},
			PricingUAKT: 100,
			Poll: ControllerPoll{
				Tick:   Duration(15 * time.Second),
				Idle:   Duration(5 * time.Minute),
				Active: Duration(60 * time.Second),
			},
		},
		Server: Server{
			Resources:     Resources{CPU: "8", Memory: "16Gi", Storage: "30Gi"},
			MemoryMax:     "8192m",
			MemoryMin:     "8192m",
			Ports:         ServerPorts{Game: 16261, UDP: 16262},
			IPLease:       true,
			IPName:        "pz-ip",
			RCON:          Feature{Enabled: false, Port: 27015},
			SSH:           Feature{Enabled: false, Port: 2222},
			Crash:         Crash{MaxRestarts: 3, Backoff: Duration(30 * time.Second)},
			OnlineTimeout: Duration(20 * time.Minute),
			PricingUAKT:   400,
		},
		Akash: Akash{
			APIBase:            "https://console-api.akash.network",
			DeployDays:         2,
			InitialDepositDays: 1,
			MaxAttempts:        15,
			BlocksPerDay:       14400,
			Price: Price{
				MaxUSDPerDay:   3.0,
				MinUSDPerDay:   0.001,
				Tolerance:      0.20,
				AKTUSDFallback: 0,
				PriceOracleURL: "https://api.coingecko.com/api/v3/simple/price?ids=akash-network&vs_currencies=usd",
			},
			Placement: Placement{
				RefLat:  52.2297,
				RefLon:  21.0122,
				SkipTTL: Duration(24 * time.Hour),
			},
			Timeouts: AkashTimeouts{
				BidPoll:       Duration(5 * time.Second),
				BidWait:       Duration(90 * time.Second),
				LeasePoll:     Duration(10 * time.Second),
				LeaseReady:    Duration(10 * time.Minute),
				DepositSettle: Duration(15 * time.Second),
			},
			Funds: Funds{
				CheckInterval: Duration(10 * time.Minute),
				MinTopupUSD:   0.5,
				Margin:        1.2,
			},
		},
		Backups: Backups{
			Dir:             "/data/backups",
			Interval:        Duration(time.Hour),
			RetentionDays:   7,
			RetentionCount:  24,
			OnHalt:          true,
			HaltTimeout:     Duration(10 * time.Minute),
			HaltConfirm:     Duration(3 * time.Minute),
			PauseFile:       "pause_autosave",
			DiskWarnPercent: 70,
			UploadMaxBytes:  2 << 30,
			// v1's behaviour, kept as the default: changing what a start does to the
			// world without being asked would be a worse surprise than the flaw.
			RestorePolicy: RestoreLatest,
		},
		DNS: DNS{
			Provider:   "cloudflare",
			Proxied:    true,
			SSLMode:    "flexible",
			IncludeWWW: true,
		},
		Game: Game{
			MaxPlayers:            32,
			PauseEmpty:            true,
			Open:                  true,
			GlobalChat:            true,
			SaveWorldEveryMinutes: 0,
			PZBackups:             PZBackups{Count: 5},
		},
		Dashboard: Dashboard{
			DefaultLocale: "en",
			Locales:       []string{"en"},
		},
		Agent: Agent{
			LivenessPush:           Duration(10 * time.Minute),
			PlayersPushMinInterval: Duration(2 * time.Minute),
			Reconcile:              Duration(20 * time.Second),
			RestoreDownloadRetries: 5,
			RestoreDownloadTimeout: Duration(30 * time.Minute),
			// Transcribed from the v1 image, so an unmodified pz-server Dockerfile
			// keeps working. Every one of these was a literal in entrypoint.sh.
			Paths: AgentPaths{
				Home:          "/home/steam",
				GameDir:       "/home/steam/pz-server",
				DataDir:       "/home/steam/Zomboid",
				LowercaseLink: "/home/steam/zomboid",
				RepoCache:     "/home/steam/pz-saves.git",
				WorkDir:       "/home/steam/work",
				LogFile:       "/home/steam/server.log",
			},
			PZ: AgentPZ{
				LaunchScripts:   []string{"start-server.sh", "StartServer64.sh"},
				ReadyBanner:     "*** SERVER STARTED ***",
				ExtraArgs:       []string{"-nosteam"},
				SaveCommand:     "save",
				QuitCommand:     "quit",
				SaveConfirm:     []string{"SAVED", "save complete", "world saved"},
				SaveTimeout:     Duration(5 * time.Minute),
				QuitTimeout:     Duration(3 * time.Minute),
				PlayersCommand:  "players",
				PlayersInterval: Duration(30 * time.Second),
			},
		},
	}
}

// Layout values for Git.Layout.
const (
	LayoutBranches = "branches"
	LayoutSingle   = "single"
)

// Load reads, decodes and validates a config file.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c, err := Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Decode applies YAML from r on top of the defaults. Unknown keys are errors.
// It does not validate; Load does that.
func Decode(r io.Reader) (*Config, error) {
	c := Defaults()
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config file is empty")
		}
		return nil, err
	}
	// A second Decode should hit EOF; more than one document is ambiguous
	// about which one wins, so reject it rather than silently using the first.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("config file contains more than one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return c, nil
}

// Find resolves which config file to use, in order:
//
//	explicit (a -c flag), $PZ_CONFIG, ./config.yaml, ./pzctl/config.yaml
func Find(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if v := os.Getenv("PZ_CONFIG"); v != "" {
		return v, nil
	}
	candidates := []string{
		DefaultFileName,
		filepath.Join("pzctl", DefaultFileName),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no config file found (looked for %s); pass -c or set PZ_CONFIG",
		strings.Join(candidates, ", "))
}

// Marshal renders the config back to YAML. Round-tripping is how `config dump`
// shows the effective configuration including every applied default.
func (c *Config) Marshal() ([]byte, error) {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}
