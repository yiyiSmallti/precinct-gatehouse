package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultKeepReleases = 5
	pendingFileName     = ".update_pending"
	failedFileName      = ".failed_update"
	currentVerFileName  = ".current_version"
	prevVerFileName     = ".previous_version"
	currentDigestFile   = ".current_digest"
	prevDigestFile      = ".previous_digest"
	deployLockFile      = ".deploy.lock"
)

// ResolveHomeDir determines the Gatehouse installation root directory.
// Priority: explicit config updater.home_dir -> parent of bin/ or releases/<ver>/
// from exePath -> directory of configPath.
func ResolveHomeDir(cfgHomeDir, configPath, exePath string) string {
	if raw := strings.TrimSpace(cfgHomeDir); raw != "" {
		clean := filepath.Clean(raw)
		if !filepath.IsAbs(clean) && strings.TrimSpace(configPath) != "" {
			clean = filepath.Join(filepath.Dir(filepath.Clean(configPath)), clean)
		}
		return filepath.Clean(clean)
	}
	if exePath != "" {
		if home := inferHomeFromBinary(exePath); home != "" {
			return home
		}
		if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
			if home := inferHomeFromBinary(resolved); home != "" {
				return home
			}
		}
	}
	if configPath != "" {
		return filepath.Dir(filepath.Clean(configPath))
	}
	return "."
}

func inferHomeFromBinary(binPath string) string {
	clean := filepath.Clean(binPath)
	parent := filepath.Dir(clean)
	if filepath.Base(parent) == "bin" {
		return filepath.Dir(parent)
	}
	grand := filepath.Dir(parent)
	if filepath.Base(grand) == "releases" {
		return filepath.Dir(grand)
	}
	return ""
}

// Store manages the release directory layout (<home>/bin, <home>/releases/<ver>,
// state files, preflight validation, atomic binary switching, and pruning).
type Store struct {
	HomeDir       string
	PreflightFunc func(ctx context.Context, binPath, configPath string) error
}

// NewStore returns a Store rooted at homeDir.
func NewStore(homeDir string) *Store {
	return &Store{HomeDir: filepath.Clean(homeDir)}
}

// BinDir returns <home>/bin.
func (s *Store) BinDir() string { return filepath.Join(s.HomeDir, "bin") }

// BinLink returns <home>/bin/gatehouse (or gatehouse.exe on Windows).
func (s *Store) BinLink() string { return filepath.Join(s.BinDir(), binaryFilename()) }

// ReleasesDir returns <home>/releases.
func (s *Store) ReleasesDir() string { return filepath.Join(s.HomeDir, "releases") }

// LockPath returns <home>/.deploy.lock.
func (s *Store) LockPath() string { return filepath.Join(s.HomeDir, deployLockFile) }

// CurrentVersion reads the active release name from the bin symlink or .current_version.
func (s *Store) CurrentVersion() string {
	if v := currentLinkVersion(s.BinLink()); v != "" {
		return v
	}
	return readTrimmedFile(filepath.Join(s.HomeDir, currentVerFileName))
}

// PreviousVersion reads .previous_version.
func (s *Store) PreviousVersion() string {
	return readTrimmedFile(filepath.Join(s.HomeDir, prevVerFileName))
}

// CurrentDigest reads .current_digest.
func (s *Store) CurrentDigest() string {
	return readTrimmedFile(filepath.Join(s.HomeDir, currentDigestFile))
}

// WriteCurrentDigest records the current mirror digest to .current_digest.
func (s *Store) WriteCurrentDigest(digest string) error {
	if err := os.MkdirAll(s.HomeDir, 0o755); err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(s.HomeDir, currentDigestFile), []byte(strings.TrimSpace(digest)+"\n"), 0o644)
}

// PreviousDigest reads .previous_digest.
func (s *Store) PreviousDigest() string {
	return readTrimmedFile(filepath.Join(s.HomeDir, prevDigestFile))
}

// ReadPending loads .update_pending if present.
func (s *Store) ReadPending() (*PendingUpdate, error) {
	b, err := os.ReadFile(filepath.Join(s.HomeDir, pendingFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p PendingUpdate
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// WritePending atomically writes .update_pending.
func (s *Store) WritePending(p *PendingUpdate) error {
	if p == nil {
		return nil
	}
	if err := os.MkdirAll(s.HomeDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(s.HomeDir, pendingFileName), append(b, '\n'), 0o644)
}

// ClearPending removes .update_pending if present.
func (s *Store) ClearPending() error {
	err := os.Remove(filepath.Join(s.HomeDir, pendingFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ReadFailed loads .failed_update if present.
func (s *Store) ReadFailed() (*FailedUpdate, error) {
	b, err := os.ReadFile(filepath.Join(s.HomeDir, failedFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f FailedUpdate
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// WriteFailed atomically writes .failed_update.
func (s *Store) WriteFailed(f *FailedUpdate) error {
	if f == nil {
		return nil
	}
	if err := os.MkdirAll(s.HomeDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(s.HomeDir, failedFileName), append(b, '\n'), 0o644)
}

// ClearFailed removes .failed_update if present.
func (s *Store) ClearFailed() error {
	err := os.Remove(filepath.Join(s.HomeDir, failedFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Preflight executes `<binPath> version` and `<binPath> config check --config <configPath>`.
func (s *Store) Preflight(ctx context.Context, binPath, configPath string) error {
	if s.PreflightFunc != nil {
		return s.PreflightFunc(ctx, binPath, configPath)
	}
	vctx, vcancel := context.WithTimeout(ctx, 5*time.Second)
	defer vcancel()
	vcmd := exec.CommandContext(vctx, binPath, "version")
	if out, err := vcmd.CombinedOutput(); err != nil {
		return fmt.Errorf("preflight version check failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(configPath) != "" {
		cctx, ccancel := context.WithTimeout(ctx, 15*time.Second)
		defer ccancel()
		ccmd := exec.CommandContext(cctx, binPath, "config", "check", "--config", configPath)
		if out, err := ccmd.CombinedOutput(); err != nil {
			return fmt.Errorf("preflight config check failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// InstallRequest describes a validated artifact ready to be installed into releases/.
type InstallRequest struct {
	Version       string
	Digest        string
	Artifact      VerifiedArtifact
	ConfigPath    string
	KeepReleases  int
	RecordPending bool
	SkipPreflight bool
}

// InstallResult summarizes the state changes produced by Install.
type InstallResult struct {
	Version         string
	PreviousVersion string
	BinarySHA256    string
}

// Install validates, stages, atomically switches bin/gatehouse to the new release,
// updates tracking files, and prunes older releases.
func (s *Store) Install(ctx context.Context, req InstallRequest) (InstallResult, error) {
	if err := ValidateVersion(req.Version); err != nil {
		return InstallResult{}, err
	}
	if _, err := os.Stat(req.Artifact.BinaryPath); err != nil {
		return InstallResult{}, fmt.Errorf("staged binary missing: %w", err)
	}
	if err := os.MkdirAll(s.HomeDir, 0o755); err != nil {
		return InstallResult{}, err
	}
	unlock, err := acquireFileLock(s.LockPath())
	if err != nil {
		return InstallResult{}, err
	}
	defer unlock()

	if err := os.MkdirAll(s.BinDir(), 0o755); err != nil {
		return InstallResult{}, err
	}
	if err := os.MkdirAll(s.ReleasesDir(), 0o755); err != nil {
		return InstallResult{}, err
	}

	if !req.SkipPreflight {
		if err := s.Preflight(ctx, req.Artifact.BinaryPath, req.ConfigPath); err != nil {
			return InstallResult{}, err
		}
	}

	if err := s.archiveBaselineIfRegularFile(); err != nil {
		return InstallResult{}, fmt.Errorf("archive baseline binary: %w", err)
	}

	prevVer := s.CurrentVersion()
	prevDigest := s.CurrentDigest()

	tmpDir, err := os.MkdirTemp(s.ReleasesDir(), fmt.Sprintf(".tmp-%s-*", req.Version))
	if err != nil {
		return InstallResult{}, err
	}
	defer func() {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	binName := binaryFilename()
	dstBin := filepath.Join(tmpDir, binName)
	if err := copyFile(req.Artifact.BinaryPath, dstBin, 0o755); err != nil {
		return InstallResult{}, err
	}
	shaHex := strings.ToLower(strings.TrimSpace(req.Artifact.BinarySHA256))
	if shaHex == "" {
		shaHex, err = SHA256File(dstBin)
		if err != nil {
			return InstallResult{}, err
		}
	}
	sumsContent := fmt.Sprintf("%s  %s\n", shaHex, binName)
	if req.Artifact.SumsPath != "" {
		if b, rerr := os.ReadFile(req.Artifact.SumsPath); rerr == nil && len(b) > 0 {
			sumsContent = string(b)
		}
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "SHA256SUMS"), []byte(sumsContent), 0o644); err != nil {
		return InstallResult{}, err
	}

	if req.Artifact.MetaPath != "" {
		if err := copyFile(req.Artifact.MetaPath, filepath.Join(tmpDir, "RELEASE_META"), 0o644); err != nil {
			return InstallResult{}, err
		}
	} else {
		meta := ReleaseMeta{
			Version:      req.Version,
			BinarySHA256: shaHex,
			BuiltAt:      time.Now().UTC().Format(time.RFC3339),
			Signed:       req.Artifact.Signed,
		}
		mb, _ := json.MarshalIndent(meta, "", "  ")
		if err := os.WriteFile(filepath.Join(tmpDir, "RELEASE_META"), append(mb, '\n'), 0o644); err != nil {
			return InstallResult{}, err
		}
	}

	if req.Artifact.SigPath != "" {
		if err := copyFile(req.Artifact.SigPath, filepath.Join(tmpDir, "SIGNATURE"), 0o644); err != nil {
			return InstallResult{}, err
		}
	}

	releaseID := s.allocateReleaseID(req.Version, req.Digest, shaHex, prevVer)
	targetDir := filepath.Join(s.ReleasesDir(), releaseID)
	if err := os.Rename(tmpDir, targetDir); err != nil {
		return InstallResult{}, err
	}
	tmpDir = ""

	if req.RecordPending {
		if err := s.WritePending(&PendingUpdate{
			TargetVersion:   releaseID,
			TargetDigest:    strings.TrimSpace(req.Digest),
			PreviousVersion: prevVer,
			PreviousDigest:  prevDigest,
			RequestedAt:     time.Now().UTC(),
		}); err != nil {
			return InstallResult{}, err
		}
	}

	relTarget := filepath.Join("..", "releases", releaseID, binName)
	if err := atomicSwapBinary(s.BinLink(), relTarget, filepath.Join(targetDir, binName)); err != nil {
		if req.RecordPending {
			_ = s.ClearPending()
		}
		return InstallResult{}, err
	}

	if prevVer != "" && prevVer != releaseID {
		_ = atomicWriteFile(filepath.Join(s.HomeDir, prevVerFileName), []byte(prevVer+"\n"), 0o644)
		if prevDigest != "" {
			_ = atomicWriteFile(filepath.Join(s.HomeDir, prevDigestFile), []byte(prevDigest+"\n"), 0o644)
		}
	}
	if err := atomicWriteFile(filepath.Join(s.HomeDir, currentVerFileName), []byte(releaseID+"\n"), 0o644); err != nil {
		return InstallResult{}, err
	}
	if strings.TrimSpace(req.Digest) != "" {
		_ = atomicWriteFile(filepath.Join(s.HomeDir, currentDigestFile), []byte(strings.TrimSpace(req.Digest)+"\n"), 0o644)
	}

	keep := req.KeepReleases
	if keep <= 0 {
		keep = defaultKeepReleases
	}
	s.pruneReleases(keep, releaseID, prevVer)

	return InstallResult{
		Version:         releaseID,
		PreviousVersion: prevVer,
		BinarySHA256:    shaHex,
	}, nil
}

func (s *Store) allocateReleaseID(ver, digest, shaHex, prevVer string) string {
	relExists := func(id string) bool {
		_, err := os.Stat(filepath.Join(s.ReleasesDir(), id))
		return !errors.Is(err, os.ErrNotExist)
	}
	if ver != prevVer && !relExists(ver) {
		return ver
	}
	suffix := shortHexSuffix(digest, shaHex)
	cand := fmt.Sprintf("%s-%s", ver, suffix)
	if cand != prevVer && !relExists(cand) {
		return cand
	}
	tsBase := fmt.Sprintf("%s-%s-%s", ver, suffix, time.Now().UTC().Format("20060102150405"))
	if tsBase != prevVer && !relExists(tsBase) {
		return tsBase
	}
	for i := 2; ; i++ {
		numbered := fmt.Sprintf("%s-%d", tsBase, i)
		if numbered != prevVer && !relExists(numbered) {
			return numbered
		}
	}
}

func shortHexSuffix(digest, shaHex string) string {
	cand := strings.TrimSpace(digest)
	if _, after, ok := strings.Cut(cand, ":"); ok {
		cand = after
	}
	if s := leadingHex8(cand); s != "" {
		return s
	}
	if s := leadingHex8(strings.TrimSpace(shaHex)); s != "" {
		return s
	}
	return "r"
}

func leadingHex8(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 8; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			b.WriteByte(c)
		case c >= 'A' && c <= 'F':
			b.WriteByte(c + ('a' - 'A'))
		default:
			return ""
		}
	}
	if b.Len() == 8 {
		return b.String()
	}
	return ""
}

// Rollback switches bin/gatehouse back to targetVersion (or .previous_version /
// newest non-current release if targetVersion is empty) after preflight checks.
func (s *Store) Rollback(ctx context.Context, targetVersion, configPath string, recordPending bool) (InstallResult, error) {
	if err := os.MkdirAll(s.HomeDir, 0o755); err != nil {
		return InstallResult{}, err
	}
	unlock, err := acquireFileLock(s.LockPath())
	if err != nil {
		return InstallResult{}, err
	}
	defer unlock()

	curVer := s.CurrentVersion()
	curDigest := s.CurrentDigest()
	target := strings.TrimSpace(targetVersion)
	if target == "" {
		target = s.resolveRollbackTarget(curVer)
	}
	if target == "" {
		return InstallResult{}, errors.New("no rollback target version available")
	}
	if err := ValidateVersion(target); err != nil {
		return InstallResult{}, err
	}

	targetBin := s.releaseBinaryPath(target)
	if targetBin == "" {
		return InstallResult{}, fmt.Errorf("target rollback binary not found for %s", target)
	}
	if err := s.Preflight(ctx, targetBin, configPath); err != nil {
		return InstallResult{}, err
	}

	prevVer := s.PreviousVersion()
	prevDigest := s.PreviousDigest()
	if recordPending {
		if err := s.WritePending(&PendingUpdate{
			TargetVersion:   target,
			TargetDigest:    prevDigest,
			PreviousVersion: curVer,
			PreviousDigest:  curDigest,
			RequestedAt:     time.Now().UTC(),
		}); err != nil {
			return InstallResult{}, err
		}
	}

	relTarget := filepath.Join("..", "releases", target, filepath.Base(targetBin))
	if err := atomicSwapBinary(s.BinLink(), relTarget, targetBin); err != nil {
		return InstallResult{}, err
	}

	if curVer != "" && curVer != target {
		_ = atomicWriteFile(filepath.Join(s.HomeDir, prevVerFileName), []byte(curVer+"\n"), 0o644)
		if curDigest != "" {
			_ = atomicWriteFile(filepath.Join(s.HomeDir, prevDigestFile), []byte(curDigest+"\n"), 0o644)
		}
	}
	if err := atomicWriteFile(filepath.Join(s.HomeDir, currentVerFileName), []byte(target+"\n"), 0o644); err != nil {
		return InstallResult{}, err
	}
	if target == prevVer && prevDigest != "" {
		_ = atomicWriteFile(filepath.Join(s.HomeDir, currentDigestFile), []byte(prevDigest+"\n"), 0o644)
	} else {
		_ = os.Remove(filepath.Join(s.HomeDir, currentDigestFile))
	}
	shaHex, _ := SHA256File(targetBin)
	return InstallResult{
		Version:         target,
		PreviousVersion: curVer,
		BinarySHA256:    shaHex,
	}, nil
}

// EmergencyRollback restores targetVersion immediately without running config preflight
// or writing .update_pending, used by the supervisor when a newly updated binary fails boot.
func (s *Store) EmergencyRollback(targetVersion, restoreDigest string) error {
	if err := ValidateVersion(targetVersion); err != nil {
		return err
	}
	unlock, err := acquireFileLock(s.LockPath())
	if err != nil {
		return err
	}
	defer unlock()

	targetBin := s.releaseBinaryPath(targetVersion)
	if targetBin == "" {
		return fmt.Errorf("rollback binary not found for %s", targetVersion)
	}
	relTarget := filepath.Join("..", "releases", targetVersion, filepath.Base(targetBin))
	if err := atomicSwapBinary(s.BinLink(), relTarget, targetBin); err != nil {
		return err
	}
	if err := atomicWriteFile(filepath.Join(s.HomeDir, currentVerFileName), []byte(targetVersion+"\n"), 0o644); err != nil {
		return err
	}
	if strings.TrimSpace(restoreDigest) != "" {
		_ = atomicWriteFile(filepath.Join(s.HomeDir, currentDigestFile), []byte(strings.TrimSpace(restoreDigest)+"\n"), 0o644)
	}
	return nil
}

func (s *Store) releaseBinaryPath(ver string) string {
	dir := filepath.Join(s.ReleasesDir(), ver)
	cand := filepath.Join(dir, binaryFilename())
	if st, err := os.Stat(cand); err == nil && !st.IsDir() {
		return cand
	}
	if binaryFilename() != "gatehouse" {
		alt := filepath.Join(dir, "gatehouse")
		if st, err := os.Stat(alt); err == nil && !st.IsDir() {
			return alt
		}
	}
	return ""
}

func (s *Store) resolveRollbackTarget(curVer string) string {
	if prev := s.PreviousVersion(); prev != "" && prev != curVer && s.releaseBinaryPath(prev) != "" {
		return prev
	}
	releases := s.ListReleases()
	for _, r := range releases {
		if r.Version != curVer && s.releaseBinaryPath(r.Version) != "" {
			return r.Version
		}
	}
	return ""
}

func (s *Store) archiveBaselineIfRegularFile() error {
	binPath := s.BinLink()
	fi, err := os.Lstat(binPath)
	if err != nil {
		return nil // missing bin/gatehouse is fine
	}
	if fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() {
		return nil
	}
	// On Windows where bin/gatehouse.exe is a regular file, only archive if no current release exists yet.
	if cur := readTrimmedFile(filepath.Join(s.HomeDir, currentVerFileName)); cur != "" && s.releaseBinaryPath(cur) != "" {
		return nil
	}
	baseVer := "baseline-" + time.Now().UTC().Format("20060102150405")
	baseDir := filepath.Join(s.ReleasesDir(), baseVer)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}
	binName := binaryFilename()
	dstBin := filepath.Join(baseDir, binName)
	if err := copyFile(binPath, dstBin, 0o755); err != nil {
		return err
	}
	shaHex, err := SHA256File(dstBin)
	if err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(baseDir, "SHA256SUMS"), []byte(fmt.Sprintf("%s  %s\n", shaHex, binName)), 0o644)
	meta := ReleaseMeta{
		Version:      baseVer,
		BinarySHA256: shaHex,
		BuiltAt:      time.Now().UTC().Format(time.RFC3339),
		Note:         "archived pre-updater baseline",
	}
	if mb, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(baseDir, "RELEASE_META"), append(mb, '\n'), 0o644)
	}
	relTarget := filepath.Join("..", "releases", baseVer, binName)
	if err := atomicSwapBinary(binPath, relTarget, dstBin); err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(s.HomeDir, currentVerFileName), []byte(baseVer+"\n"), 0o644)
}

func (s *Store) pruneReleases(keepCount int, keepCur, keepPrev string) {
	entries, err := os.ReadDir(s.ReleasesDir())
	if err != nil {
		return
	}
	type item struct {
		mtime time.Time
		path  string
	}
	var cands []item
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "baseline-") {
			continue
		}
		if name == keepCur || name == keepPrev {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, item{mtime: info.ModTime(), path: filepath.Join(s.ReleasesDir(), name)})
	}
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].mtime.After(cands[j].mtime)
	})
	if len(cands) <= keepCount {
		return
	}
	for _, old := range cands[keepCount:] {
		_ = os.RemoveAll(old.path)
	}
}

// ListReleases lists installed release directories sorted newest first.
func (s *Store) ListReleases() []ReleaseInfo {
	cur := s.CurrentVersion()
	prev := s.PreviousVersion()
	entries, err := os.ReadDir(s.ReleasesDir())
	if err != nil {
		return []ReleaseInfo{}
	}
	type sortable struct {
		mtime time.Time
		info  ReleaseInfo
	}
	var items []sortable
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(s.ReleasesDir(), name)
		fi, err := e.Info()
		if err != nil {
			continue
		}
		ri := ReleaseInfo{
			Version:    name,
			Current:    name == cur,
			Previous:   name == prev,
			Baseline:   strings.HasPrefix(name, "baseline-"),
			ModifiedAt: fi.ModTime().UTC().Format(time.RFC3339),
		}
		if mb, err := os.ReadFile(filepath.Join(full, "RELEASE_META")); err == nil {
			var meta ReleaseMeta
			if json.Unmarshal(mb, &meta) == nil {
				ri.BinarySHA256 = meta.BinarySHA256
				ri.ShortSHA = meta.ShortSHA
				ri.BuiltAt = meta.BuiltAt
				ri.Signed = meta.Signed
				ri.Note = meta.Note
			}
		}
		if !ri.Signed {
			if _, err := os.Stat(filepath.Join(full, "SIGNATURE")); err == nil {
				ri.Signed = true
			}
		}
		items = append(items, sortable{mtime: fi.ModTime(), info: ri})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].info.Current != items[j].info.Current {
			return items[i].info.Current
		}
		return items[i].mtime.After(items[j].mtime)
	})
	out := make([]ReleaseInfo, 0, len(items))
	for _, it := range items {
		out = append(out, it.info)
	}
	return out
}

func readTrimmedFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	return os.Chmod(dst, perm)
}
