package astrlink

import (
	"fmt"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

var (
	initOnce sync.Once
	initErr  error
)

// InitRuntime loads the ONNX Runtime shared library once per process. The
// Go binding (onnxruntime_go v1.31.0) is built against ORT C API version 26,
// so the library must be ONNX Runtime 1.26 or newer.
func InitRuntime(libPath string) error {
	initOnce.Do(func() {
		ort.SetSharedLibraryPath(libPath)
		initErr = ort.InitializeEnvironment()
	})
	return initErr
}

// Model is one ONNX Runtime session of the token classifier. Run is safe for
// concurrent use.
type Model struct {
	sess      *ort.DynamicAdvancedSession
	NumLabels int
}

// LoadModel opens an ONNX file (external weights must sit next to it).
// threads sets intra-op parallelism; 0 keeps ORT's default (all cores).
func LoadModel(path string, numLabels, threads int) (*Model, error) {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	if threads > 0 {
		if err := opts.SetIntraOpNumThreads(threads); err != nil {
			return nil, err
		}
		if err := opts.SetInterOpNumThreads(1); err != nil {
			return nil, err
		}
	}
	s, err := ort.NewDynamicAdvancedSession(path, []string{"input_ids", "attention_mask", "token_type_ids"}, []string{"logits"}, opts)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return &Model{sess: s, NumLabels: numLabels}, nil
}

// Close releases the session.
func (m *Model) Close() error { return m.sess.Destroy() }

// Run scores a batch of windows. Each window is a full input sequence
// (<s> … </s>); shorter windows are padded with <pad> and mask 0. It returns
// logits[b][t*NumLabels+k] for the real (unpadded) positions.
func (m *Model) Run(windows [][]int32) ([][]float32, error) {
	b := len(windows)
	l := 0
	for _, w := range windows {
		l = max(l, len(w))
	}
	ids := make([]int64, b*l)
	mask := make([]int64, b*l)
	for i, w := range windows {
		for j := 0; j < l; j++ {
			if j < len(w) {
				ids[i*l+j], mask[i*l+j] = int64(w[j]), 1
			} else {
				ids[i*l+j] = PAD
			}
		}
	}
	shape := ort.NewShape(int64(b), int64(l))
	tIDs, err := ort.NewTensor(shape, ids)
	if err != nil {
		return nil, err
	}
	defer tIDs.Destroy()
	tMask, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, err
	}
	defer tMask.Destroy()
	tType, err := ort.NewTensor(shape, make([]int64, b*l))
	if err != nil {
		return nil, err
	}
	defer tType.Destroy()
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(int64(b), int64(l), int64(m.NumLabels)))
	if err != nil {
		return nil, err
	}
	defer out.Destroy()
	if err := m.sess.Run([]ort.Value{tIDs, tMask, tType}, []ort.Value{out}); err != nil {
		return nil, err
	}
	data := out.GetData()
	res := make([][]float32, b)
	for i, w := range windows {
		row := data[i*l*m.NumLabels : (i*l+len(w))*m.NumLabels]
		res[i] = append([]float32(nil), row...)
	}
	return res, nil
}
