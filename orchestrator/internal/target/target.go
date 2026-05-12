package target

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
	"sigs.k8s.io/yaml"
)

const debounceDuration = 100 * time.Millisecond

type Target struct {
	Shares     map[string]float64 `yaml:"shares" json:"shares"`
	TotalBytes int64              `yaml:"total_bytes" json:"total_bytes"`
	SHA256     string             `yaml:"-" json:"-"`
}

func Load(path string) (*Target, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("target: read %s: %w", path, err)
	}
	return parse(raw)
}

func parse(raw []byte) (*Target, error) {
	var t Target
	if err := yaml.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("target: parse yaml: %w", err)
	}
	sum := 0.0
	for _, v := range t.Shares {
		sum += v
	}
	if len(t.Shares) > 0 && abs(sum-1.0) > 0.01 {
		return nil, fmt.Errorf("target: shares sum to %.6f, must be 1.0 (±0.01)", sum)
	}
	digest := sha256.Sum256(raw)
	t.SHA256 = hex.EncodeToString(digest[:])
	return &t, nil
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

type Watcher struct {
	fw       *fsnotify.Watcher
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewWatcher(ctx context.Context, path string, onChange func(*Target)) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("target: create fsnotify watcher: %w", err)
	}
	if err := fw.Add(path); err != nil {
		fw.Close()
		return nil, fmt.Errorf("target: watch %s: %w", path, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	w := &Watcher{
		fw:     fw,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go w.loop(ctx, path, onChange)
	return w, nil
}

func (w *Watcher) loop(ctx context.Context, path string, onChange func(*Target)) {
	defer close(w.done)
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-w.fw.Events:
			if !ok {
				return
			}
			debounce = time.After(debounceDuration)
		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			slog.Error("target watcher error", "err", err)
		case <-debounce:
			debounce = nil
			t, err := Load(path)
			if err != nil {
				slog.Error("target reload failed", "path", path, "err", err)
				continue
			}
			onChange(t)
		}
	}
}

func (w *Watcher) Close() error {
	w.cancel()
	err := w.fw.Close()
	<-w.done
	return err
}
