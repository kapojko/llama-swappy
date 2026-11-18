package monitor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func f64p(v float64) *float64 { return &v }
func u64p(v uint64) *uint64 { return &v }

func TestCombineAvgAndMax(t *testing.T) {
	base := time.Now()
	mk := func(tempA, tempB *float64, util *float64) Sample {
		return Sample{
			Type: SampleType, TS: base, Model: "m1",
			GPUs: []GPUSample{{Index: 0, Name: "g", TempC: tempA, UtilPct: util},
				{Index: 1, Name: "g", TempC: tempB}},
			NVMes: []NVMESample{{Name: "nvme0n1", ReadBps: f64p(10)}},
		}
	}
	a := mk(f64p(40), f64p(50), f64p(10))
	b := mk(f64p(60), nil, f64p(30))

	avg := combine([]Sample{a, b}, combineAvg)
	if got := *avg.GPUs[0].TempC; got != 50 {
		t.Errorf("avg GPU0 temp = %v, want 50", got)
	}
	if got := *avg.GPUs[0].UtilPct; got != 20 {
		t.Errorf("avg GPU0 util = %v, want 20", got)
	}
	// GPU1 temp is present in one sample only: avg over that one.
	if got := *avg.GPUs[1].TempC; got != 50 {
		t.Errorf("avg GPU1 temp = %v, want 50", got)
	}

	max := combine([]Sample{a, b}, combineMax)
	if got := *max.GPUs[0].TempC; got != 60 {
		t.Errorf("max GPU0 temp = %v, want 60", got)
	}
	if got := *max.GPUs[0].UtilPct; got != 30 {
		t.Errorf("max GPU0 util = %v, want 30", got)
	}
	if got := *max.NVMes[0].ReadBps; got != 10 {
		t.Errorf("max nvme read = %v, want 10", got)
	}
	if max.Model != "m1" || max.Type != SampleType {
		t.Errorf("max meta = %q/%q, want m1/sample", max.Model, max.Type)
	}
}

func TestCombinePairsDevicesAcrossSamples(t *testing.T) {
	base := time.Now()
	// Sample 1 knows gpu 0 and 1; sample 2 only gpu 0 (the second
	// card disappeared): the rows must not be merged.
	s1 := Sample{TS: base, GPUs: []GPUSample{{Index: 0, Name: "a", TempC: f64p(1)}, {Index: 1, Name: "b", TempC: f64p(2)}}}
	s2 := Sample{TS: base, GPUs: []GPUSample{{Index: 0, Name: "a", TempC: f64p(3)}}}
	res := combine([]Sample{s1, s2}, combineMax)
	if len(res.GPUs) != 2 {
		t.Fatalf("GPUs = %+v, want 2 rows", res.GPUs)
	}
	if *res.GPUs[0].TempC != 3 || *res.GPUs[1].TempC != 2 {
		t.Errorf("temps = %v/%v, want 3/2", *res.GPUs[0].TempC, *res.GPUs[1].TempC)
	}
}

func TestCombineDoesNotMutateSamples(t *testing.T) {
	base := time.Now()
	v := 10.0
	s := Sample{TS: base, GPUs: []GPUSample{{Index: 0, Name: "g", TempC: &v}}}
	combine([]Sample{s, {TS: base, GPUs: []GPUSample{{Index: 0, Name: "g", TempC: f64p(20)}}}}, combineMax)
	if v != 10 {
		t.Errorf("source sample mutated: temp now %v, want 10", v)
	}
}

func TestCombineEmpty(t *testing.T) {
	if res := combine(nil, combineAvg); res.Type != SampleType {
		t.Errorf("combine(nil) = %+v, want an empty sample", res)
	}
}

func TestDecodeSamples(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor-2026-01-02.jsonl")
	content := `{"type":"sample","ts":"2026-01-02T10:00:00Z","gpus":[{"index":0,"name":"g","tempC":50}],"model":"idle"}` + "\n" +
		`{"type":"model_loaded","ts":"2026-01-02T10:00:01Z","model":"m1"}` + "\n" +
		`{"type":"sample","ts":"2026-01-02T10:00:05Z","gpus":[{"index":0,"name":"g","tempC":51}],"model":"m1"}` + "\n" +
		`{"type":"sample","ts":"2026-01-02T10:00:09Z","gpus":[{"index":0,"nam` // truncated tail
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	samples, err := decodeSamples(path)
	if err != nil {
		t.Fatalf("decodeSamples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2 (events and truncated line skipped)", len(samples))
	}
	if samples[0].GPUs[0].TempC == nil || *samples[0].GPUs[0].TempC != 50 {
		t.Errorf("first sample = %+v", samples[0])
	}
	if samples[1].Model != "m1" {
		t.Errorf("second sample model = %q, want m1", samples[1].Model)
	}
}
