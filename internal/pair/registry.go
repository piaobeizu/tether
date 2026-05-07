package pair

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// DeviceRecord is the on-disk shape per spec §9.2 — one of these per
// peer device, written to ~/.tether/users/<user>/devices/<deviceId>.json
// at file mode 0600. Fields not provided by the v0.1 wire (model,
// osVersion, appVersion) are kept as optional strings; the daemon may
// populate them later without a file-format bump because the JSON
// decoder ignores absent fields.
type DeviceRecord struct {
	V                   int            `json:"v"`
	DeviceID            DeviceID       `json:"deviceId"`
	Kind                DeviceKind     `json:"kind"`
	DisplayName         string         `json:"displayName"`
	Model               string         `json:"model,omitempty"`
	LongTermKey         []byte         `json:"-"` // serialized as base64url string
	TransportBindingKey []byte         `json:"-"` // serialized as base64url string
	LongTermKeyID       string         `json:"longTermKeyId,omitempty"`
	PushToken           string         `json:"-"` // serialized inside pushToken object
	PairedAt            time.Time      `json:"-"` // serialized as ISO 8601 UTC ms
	LastSeen            time.Time      `json:"-"` // serialized as ISO 8601 UTC ms
}

// jsonDeviceRecord is the wire shape — separate from DeviceRecord so
// we can format []byte → base64url and time.Time → ISO 8601 without
// custom MarshalJSON on the public struct.
type jsonDeviceRecord struct {
	V                   int        `json:"v"`
	DeviceID            DeviceID   `json:"deviceId"`
	Kind                DeviceKind `json:"kind"`
	DisplayName         string     `json:"displayName"`
	Model               string     `json:"model,omitempty"`
	LongTermKey         string     `json:"longTermKey"`
	TransportBindingKey string     `json:"transportBindingKey"`
	LongTermKeyID       string     `json:"longTermKeyId,omitempty"`
	PushToken           *struct {
		Type    string `json:"type"`
		Payload struct {
			Token string `json:"token"`
		} `json:"payload"`
	} `json:"pushToken,omitempty"`
	PairedAt string `json:"pairedAt"`
	LastSeen string `json:"lastSeen"`
}

// deviceIDRegexp is the constraint from spec §9.1: filenames are
// [a-zA-Z0-9-]+ and must be at least 1 char.
var deviceIDRegexp = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

// ValidateDeviceID returns nil iff id is a valid filename per §9.1.
func ValidateDeviceID(id DeviceID) error {
	if id == "" {
		return errors.New("pair: empty deviceId")
	}
	if !deviceIDRegexp.MatchString(string(id)) {
		return fmt.Errorf("pair: deviceId %q has invalid characters (allowed: [a-zA-Z0-9-]+)", string(id))
	}
	return nil
}

// Registry persists per-device records under
// ~/.tether/users/<user>/devices/. v0.1 hardcodes user="default".
//
// Concurrency: Save / ForceSave / Delete are mutex-serialized to
// prevent two paired devices from racing on the same file. Load / List
// take the mutex briefly to avoid reading a half-written file.
type Registry struct {
	root  string // ~/.tether/users/<user>/devices/
	mu    sync.Mutex
	audit *AuditLog
}

// RegistryConfig is the explicit-config form of NewRegistry. Root is
// the per-user devices directory; AuditLog is the (optional) sink
// that ForceSave writes pair.force-rotated lines into. If AuditLog is
// nil, ForceSave still rotates but does not log.
type RegistryConfig struct {
	Root  string
	Audit *AuditLog
}

// NewRegistry opens (or creates) a Registry at the given directory.
// Parent dirs are created at 0700 if missing.
func NewRegistry(cfg RegistryConfig) (*Registry, error) {
	if cfg.Root == "" {
		return nil, errors.New("pair: registry root path required")
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("pair: mkdir registry %q: %w", cfg.Root, err)
	}
	// Tighten mode in case dir pre-existed with looser perms.
	_ = os.Chmod(cfg.Root, 0o700)
	return &Registry{root: cfg.Root, audit: cfg.Audit}, nil
}

// DefaultRegistryRoot returns ~/.tether/users/<user>/devices/. user is
// the v0.1 hardcoded "default"; v0.2 will let callers pass a real id.
func DefaultRegistryRoot(home, user string) string {
	if user == "" {
		user = DefaultUser
	}
	return filepath.Join(home, ".tether", "users", user, "devices")
}

// ErrAlreadyPaired is returned by Save when a record already exists
// for the deviceId. Per spec §14 Q2 ratification the default re-pair
// behavior is reject; operators must explicitly call ForceSave to
// overwrite.
var ErrAlreadyPaired = errors.New("pair: device already paired")

// ErrNotFound is returned by Load when no record exists for the id.
var ErrNotFound = errors.New("pair: device record not found")

// Save writes a DeviceRecord at <root>/<deviceId>.json at mode 0600.
// Returns ErrAlreadyPaired if a record already exists. Atomic via
// tmp + rename.
func (r *Registry) Save(rec DeviceRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked(rec, false)
}

// ForceSave overwrites any existing record and emits a
// pair.force-rotated audit line (if AuditLog is configured). Returns
// only IO errors.
func (r *Registry) ForceSave(rec DeviceRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Capture old longTermKeyId for the audit line.
	prevID := ""
	if existing, err := r.loadLocked(rec.DeviceID); err == nil {
		prevID = existing.LongTermKeyID
	}
	if err := r.saveLocked(rec, true); err != nil {
		return err
	}
	if r.audit != nil {
		_ = r.audit.AppendForceRotated(rec.DeviceID, rec.LongTermKeyID, prevID)
	}
	return nil
}

func (r *Registry) saveLocked(rec DeviceRecord, force bool) error {
	if err := ValidateDeviceID(rec.DeviceID); err != nil {
		return err
	}
	path := r.path(rec.DeviceID)
	if !force {
		if _, err := os.Stat(path); err == nil {
			// Audit the rejection so operators can observe re-pair
			// attempts (§10.3). Best-effort; we don't fail Save on
			// audit IO error.
			if r.audit != nil {
				_ = r.audit.AppendRepairRejected(rec.DeviceID)
			}
			return ErrAlreadyPaired
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("pair: stat %q: %w", path, err)
		}
	}
	row := toJSONRecord(rec)
	body, err := json.MarshalIndent(row, "", "  ")
	if err != nil {
		return fmt.Errorf("pair: marshal device record: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("pair: write tmp %q: %w", tmp, err)
	}
	// Tighten mode in case the file pre-existed with looser perms.
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		// Best-effort cleanup.
		_ = os.Remove(tmp)
		return fmt.Errorf("pair: rename %q -> %q: %w", tmp, path, err)
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// Load reads the on-disk record for deviceID. Returns ErrNotFound if
// no record exists.
func (r *Registry) Load(deviceID DeviceID) (DeviceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked(deviceID)
}

func (r *Registry) loadLocked(deviceID DeviceID) (DeviceRecord, error) {
	if err := ValidateDeviceID(deviceID); err != nil {
		return DeviceRecord{}, err
	}
	path := r.path(deviceID)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DeviceRecord{}, ErrNotFound
		}
		return DeviceRecord{}, fmt.Errorf("pair: read %q: %w", path, err)
	}
	var row jsonDeviceRecord
	if err := json.Unmarshal(body, &row); err != nil {
		return DeviceRecord{}, fmt.Errorf("pair: parse %q: %w", path, err)
	}
	return fromJSONRecord(row)
}

// List returns the deviceIDs of every record currently on disk.
// Sorted alphabetically for deterministic test output.
func (r *Registry) List() ([]DeviceID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := os.ReadDir(r.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("pair: read registry %q: %w", r.root, err)
	}
	var ids []DeviceID
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		// Only count files that look like <deviceId>.json. Skip .tmp
		// crash leftovers, hidden files, etc.
		if filepath.Ext(name) != ".json" {
			continue
		}
		id := DeviceID(name[:len(name)-len(".json")])
		if ValidateDeviceID(id) != nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// Delete removes a device record. Returns ErrNotFound if no such
// record. Used by the CLI rotate-deviceid path.
func (r *Registry) Delete(deviceID DeviceID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ValidateDeviceID(deviceID); err != nil {
		return err
	}
	path := r.path(deviceID)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("pair: remove %q: %w", path, err)
	}
	return nil
}

func (r *Registry) path(id DeviceID) string {
	return filepath.Join(r.root, string(id)+".json")
}

func toJSONRecord(rec DeviceRecord) jsonDeviceRecord {
	row := jsonDeviceRecord{
		V:                   1,
		DeviceID:            rec.DeviceID,
		Kind:                rec.Kind,
		DisplayName:         rec.DisplayName,
		Model:               rec.Model,
		LongTermKey:         b64uEncode(rec.LongTermKey),
		TransportBindingKey: b64uEncode(rec.TransportBindingKey),
		LongTermKeyID:       rec.LongTermKeyID,
		PairedAt:            rec.PairedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		LastSeen:            rec.LastSeen.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
	if rec.PushToken != "" {
		var pt struct {
			Type    string `json:"type"`
			Payload struct {
				Token string `json:"token"`
			} `json:"payload"`
		}
		pt.Type = "fcm"
		pt.Payload.Token = rec.PushToken
		row.PushToken = &pt
	}
	return row
}

func fromJSONRecord(row jsonDeviceRecord) (DeviceRecord, error) {
	ltk, err := b64uDecode(row.LongTermKey)
	if err != nil {
		return DeviceRecord{}, fmt.Errorf("pair: decode longTermKey: %w", err)
	}
	tbk, err := b64uDecode(row.TransportBindingKey)
	if err != nil {
		return DeviceRecord{}, fmt.Errorf("pair: decode transportBindingKey: %w", err)
	}
	pairedAt, _ := time.Parse(time.RFC3339Nano, row.PairedAt)
	lastSeen, _ := time.Parse(time.RFC3339Nano, row.LastSeen)
	rec := DeviceRecord{
		V:                   row.V,
		DeviceID:            row.DeviceID,
		Kind:                row.Kind,
		DisplayName:         row.DisplayName,
		Model:               row.Model,
		LongTermKey:         ltk,
		TransportBindingKey: tbk,
		LongTermKeyID:       row.LongTermKeyID,
		PairedAt:            pairedAt,
		LastSeen:            lastSeen,
	}
	if row.PushToken != nil {
		rec.PushToken = row.PushToken.Payload.Token
	}
	return rec, nil
}
