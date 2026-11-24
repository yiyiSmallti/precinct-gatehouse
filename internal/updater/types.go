package updater

import (
	"context"
	"time"
)

// ExitRestart is returned by `gatehouse serve` when exiting specifically so
// `gatehouse supervise` restarts it immediately onto a newly swapped release.
const ExitRestart = 75

// EnvSupervised is set to "1" by `gatehouse supervise` for child processes.
const EnvSupervised = "GATEHOUSE_SUPERVISED"

// EnvHomeDir passes the supervisor's resolved --home directory to child processes.
const EnvHomeDir = "GATEHOUSE_HOME"

// EnvBootNonce, HeaderBootProbe, and HeaderBootAck bind each supervised child boot
// to its supervisor's post-update /healthz verification probe so an unrelated process
// listening on the same address cannot forge readiness.
const (
	EnvBootNonce    = "GATEHOUSE_BOOT_NONCE"
	HeaderBootProbe = "X-Gatehouse-Boot-Probe"
	HeaderBootAck   = "X-Gatehouse-Boot-Ack"
)

// Update state constants reported via Manager.Status() and Admin API.
const (
	StateIdle        = "idle"
	StateChecking    = "checking"
	StateAvailable   = "available"
	StateDownloading = "downloading"
	StateVerifying   = "verifying"
	StateDraining    = "draining"
	StateApplying    = "applying"
	StateRestarting  = "restarting"
	StateError       = "error"
)

// RemoteInfo describes the latest release discovered on a remote update source.
type RemoteInfo struct {
	Source          string `json:"source"`
	Version         string `json:"version,omitempty"`
	Digest          string `json:"digest,omitempty"`
	Tag             string `json:"tag,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	Notes           string `json:"notes,omitempty"`
	URL             string `json:"url,omitempty"`
	Commit          string `json:"commit,omitempty"`
	Signed          bool   `json:"signed,omitempty"`
	AssetName       string `json:"asset_name,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
}

// DownloadedArtifact holds the verified payload ready for preflight & install.
type DownloadedArtifact struct {
	Remote   RemoteInfo
	Verified VerifiedArtifact
	Version  string
}

// ArtifactSource abstracts OCI Registry v2 and GitHub Releases backends.
type ArtifactSource interface {
	Check(ctx context.Context) (*RemoteInfo, error)
	Download(ctx context.Context, outDir string) (*DownloadedArtifact, error)
}

// PendingUpdate is persisted to .update_pending before swapping releases so the
// supervisor (or direct startup) can verify health and roll back on boot failure.
type PendingUpdate struct {
	TargetVersion   string    `json:"target_version"`
	TargetDigest    string    `json:"target_digest,omitempty"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	PreviousDigest  string    `json:"previous_digest,omitempty"`
	RequestedAt     time.Time `json:"requested_at"`
}

// FailedUpdate is persisted to .failed_update after an automatic rollback so
// background polling does not crash-loop on the same broken release.
type FailedUpdate struct {
	Version string    `json:"version"`
	Digest  string    `json:"digest,omitempty"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// ReleaseInfo summarizes one release directory stored under <home>/releases/.
type ReleaseInfo struct {
	Version      string `json:"version"`
	BinarySHA256 string `json:"binary_sha256,omitempty"`
	ShortSHA     string `json:"short_sha,omitempty"`
	BuiltAt      string `json:"built_at,omitempty"`
	Signed       bool   `json:"signed,omitempty"`
	HasSignature bool   `json:"has_signature,omitempty"`
	Note         string `json:"note,omitempty"`
	Current      bool   `json:"current"`
	Previous     bool   `json:"previous"`
	Baseline     bool   `json:"baseline,omitempty"`
	ModifiedAt   string `json:"modified_at,omitempty"`
}

// UpdateEvent records one updater check, apply, rollback, or error entry.
type UpdateEvent struct {
	At      time.Time `json:"at"`
	Action  string    `json:"action"` // check | apply | rollback | auto_rollback | error
	Version string    `json:"version,omitempty"`
	Digest  string    `json:"digest,omitempty"`
	Status  string    `json:"status"` // ok | skipped | failed
	Detail  string    `json:"detail,omitempty"`
}

// ConfigSummary presents sanitized updater configuration for Status/Admin UI.
type ConfigSummary struct {
	Enabled          bool     `json:"enabled"`
	AutoApply        bool     `json:"auto_apply"`
	Source           string   `json:"source"`
	Interval         string   `json:"interval"`
	DrainTimeout     string   `json:"drain_timeout"`
	HealthTimeout    string   `json:"health_timeout"`
	KeepReleases     int      `json:"keep_releases"`
	HomeDir          string   `json:"home_dir,omitempty"`
	FallbackDNS      []string `json:"fallback_dns"`
	HasPublicKey     bool     `json:"has_public_key"`
	PublicKey        string   `json:"public_key,omitempty"`
	RequireSignature bool     `json:"require_signature"`
	Image            string   `json:"image,omitempty"`
	Tag              string   `json:"tag,omitempty"`
	Username         string   `json:"username,omitempty"`
	HasPassword      bool     `json:"has_password"`
	GitHubRepo       string   `json:"github_repo,omitempty"`
	GitHubAPIURL     string   `json:"github_api_url,omitempty"`
	HasToken         bool     `json:"has_token"`
	Prerelease       bool     `json:"prerelease"`
}

// Status is the complete snapshot returned by Manager.Status() and GET /admin/api/updater.
type Status struct {
	CurrentVersion  string         `json:"current_version"`
	ActiveRelease   string         `json:"active_release,omitempty"`
	PreviousRelease string         `json:"previous_release,omitempty"`
	CurrentDigest   string         `json:"current_digest,omitempty"`
	HomeDir         string         `json:"home_dir"`
	Supervised      bool           `json:"supervised"`
	CanRestart      bool           `json:"can_restart"`
	State           string         `json:"state"`
	Message         string         `json:"message,omitempty"`
	Inflight        int64          `json:"inflight"`
	Available       *RemoteInfo    `json:"available,omitempty"`
	Pending         *PendingUpdate `json:"pending,omitempty"`
	Failed          *FailedUpdate  `json:"failed_update,omitempty"`
	LastCheckAt     *time.Time     `json:"last_check_at,omitempty"`
	NextCheckAt     *time.Time     `json:"next_check_at,omitempty"`
	LastError       string         `json:"last_error,omitempty"`
	Releases        []ReleaseInfo  `json:"releases"`
	History         []UpdateEvent  `json:"history"`
	Config          ConfigSummary  `json:"config"`
}
