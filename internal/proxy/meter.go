package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// newBootID generates a random 16-hex-char boot identifier. It is generated
// ONLY when the persisted file is missing (a fresh boot); a container
// restart re-reads the file's bootID instead of regenerating one (P1-B). A
// random byte string is used rather than a UUID dependency to keep the proxy
// a single self-contained Go module.
func newBootID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// usageFile is the on-disk shape of the persisted metering state. A container
// restart re-reads the same file and keeps BOTH the counters and the bootID,
// so the operator's delta-from-0 (P2d) is continuous across a crash. A pod
// recreate wipes the emptyDir, so a fresh boot gets a new bootID with
// counters from 0 (the operator treats that as a fresh start).
type usageFile struct {
	BootID            string `json:"bootID"`
	PromptTokens      int64  `json:"promptTokens"`
	CompletionTokens  int64  `json:"completionTokens"`
	Requests          int64  `json:"requests"`
	UnmeteredRequests int64  `json:"unmeteredRequests"`
}

// Reading is the cumulative reading the /coxswain/usage endpoint returns. It
// is cumulative, not a delta — the operator adds deltas (P2d).
type Reading struct {
	BootID            string `json:"bootID"`
	PromptTokens      int64  `json:"promptTokens"`
	CompletionTokens  int64  `json:"completionTokens"`
	Requests          int64  `json:"requests"`
	UnmeteredRequests int64  `json:"unmeteredRequests"`
	Model             string `json:"model"`
	SinceStart        string `json:"sinceStart"`
}

// Meter holds the per-Loop cumulative token counters. It is pod-scoped (one
// pod per Loop by the D33 topology) and persisted to an emptyDir file so a
// container restart does not reset the counts (P1-B: the bootID is persisted
// too, generated only when the file is missing).
type Meter struct {
	mu    sync.Mutex
	path  string
	model string
	boot  time.Time

	file usageFile
}

// NewMeter loads the persisted state from path (re-reading the counters and
// the bootID on a container restart) or, when the file is absent (a fresh
// boot / a pod recreate with a wiped emptyDir), generates a new bootID and
// starts at zero. The model is the Loop's model name for the reading; since
// is the boot wall-clock timestamp the reading reports.
func NewMeter(path, model string, since time.Time) (*Meter, error) {
	m := &Meter{path: path, model: model, boot: since}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Fresh boot: generate the bootID once. A container restart finds
			// the file and reuses its bootID instead of regenerating.
			m.file = usageFile{BootID: newBootID()}
			return m, nil
		}
		// A readable-but-corrupt file is a hard error: writing over it would
		// silently lose the counters. Fail loudly at boot.
		return nil, err
	}
	if err := json.Unmarshal(data, &m.file); err != nil {
		return nil, err
	}
	if m.file.BootID == "" {
		m.file.BootID = newBootID()
	}
	return m, nil
}

// Add metered increments the cumulative counters by the reported usage and
// bumps the request count. A response WITHOUT a usage object is Add(0, 0).
func (m *Meter) Add(prompt, completion int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.file.PromptTokens += prompt
	m.file.CompletionTokens += completion
	m.file.Requests++
	_ = m.persistLocked()
}

// AddUnmetered increments the unmetered-request count (a body with no parseable
// usage object, or a stream that ends without a usage chunk) and the request
// count. The token counters are untouched — an under-count is visible as
// unmeteredRequests > 0, never an over-count.
func (m *Meter) AddUnmetered() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.file.UnmeteredRequests++
	m.file.Requests++
	_ = m.persistLocked()
}

// Reading returns the current cumulative reading.
func (m *Meter) Reading() Reading {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Reading{
		BootID:            m.file.BootID,
		PromptTokens:      m.file.PromptTokens,
		CompletionTokens:  m.file.CompletionTokens,
		Requests:          m.file.Requests,
		UnmeteredRequests: m.file.UnmeteredRequests,
		Model:             m.model,
		SinceStart:        m.boot.UTC().Format(time.RFC3339),
	}
}

// persistLocked writes the file atomically (temp + rename) so a concurrent
// reader never observes a torn file. Callers must hold m.mu. The write error
// is swallowed: the in-memory counters are the source of truth and the next
// metered request re-persists; a persistent I/O failure surfaces at boot on
// the next restart instead.
func (m *Meter) persistLocked() error {
	if m.path == "" {
		return nil
	}
	data, err := json.Marshal(m.file)
	if err != nil {
		return err
	}
	dir := dirOf(m.path)
	tmp, err := os.CreateTemp(dir, ".usage-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), m.path)
}

func dirOf(path string) string {
	for i := len(path) - 1; i > 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
