package monitor

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"
)

// Statistics over samples: the last-minute average comes from the
// in-memory ring, the daily and weekly maxima from the JSONL log
// files. Both use the same reduction, which walks a Sample and
// combines every numeric field independently: a field missing from
// some samples is aggregated over the samples where it is present,
// and a field missing everywhere stays missing.
//
// GPU samples are paired by Index and NVMe samples by Name across
// samples (the "0-N devices" rule: devices appearing later or
// disappearing mid-window stay in their own rows).

type combineMode int

const (
	combineAvg combineMode = iota
	combineMax
)

// numAgg accumulates one numeric field across samples.
type numAgg struct {
	count int
	sum   float64
	max   float64
}

func (a *numAgg) add(v float64) {
	a.count++
	a.sum += v
	if a.count == 1 || v > a.max {
		a.max = v
	}
}

// combine reduces samples into one Sample: every numeric field holds
// the average or the maximum of that field across the samples. TS and
// Model come from the last sample (the reduction describes a window,
// so the timestamp is where it ends). An empty input yields the zero
// Sample.
func combine(samples []Sample, mode combineMode) Sample {
	var out Sample
	if len(samples) == 0 {
		out.Type = SampleType
		return out
	}
	acc := map[string]*numAgg{}
	dst := reflect.ValueOf(&out).Elem()
	for i := range samples {
		mergeInto(dst, reflect.ValueOf(&samples[i]).Elem(), "", acc)
	}
	finalize(dst, "", acc, mode)
	out.TS = samples[len(samples)-1].TS
	out.Model = samples[len(samples)-1].Model
	out.Type = SampleType
	return out
}

// float64Type is the conversion target for accumulating numeric
// fields of any Go number kind.
var float64Type = reflect.TypeOf(float64(0))

// mergeInto walks src alongside dst, allocating dst as needed, and
// accumulates every pointer-to-number field of src into acc, keyed by
// the field path. Slice elements are paired by their key field
// (Index or Name). dst is never made to share memory with src.
func mergeInto(dst, src reflect.Value, path string, acc map[string]*numAgg) {
	switch dst.Kind() {
	case reflect.Struct:
		for i := 0; i < dst.NumField(); i++ {
			if !dst.Type().Field(i).IsExported() {
				continue
			}
			mergeInto(dst.Field(i), src.Field(i), path+"/"+dst.Type().Field(i).Name, acc)
		}
	case reflect.Ptr:
		if src.IsNil() {
			return
		}
		if isNumberKind(dst.Type().Elem().Kind()) {
			// Accumulate only; the value is written by finalize, so
			// the samples' own storage is never mutated.
			a := acc[path]
			if a == nil {
				a = &numAgg{}
				acc[path] = a
			}
			a.add(src.Elem().Convert(float64Type).Float())
			return
		}
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		mergeInto(dst.Elem(), src.Elem(), path, acc)
	case reflect.Slice:
		for j := 0; j < src.Len(); j++ {
			key, ok := elemKey(src.Index(j))
			if !ok {
				continue
			}
			var found reflect.Value
			for k := 0; k < dst.Len(); k++ {
				if dk, ok := elemKey(dst.Index(k)); ok && dk == key {
					found = dst.Index(k)
					break
				}
			}
			if !found.IsValid() {
				dst.Set(reflect.Append(dst, deepCopyValue(src.Index(j))))
				found = dst.Index(dst.Len() - 1)
			}
			mergeInto(found, src.Index(j), path+"["+key+"]", acc)
		}
	}
}

// finalize writes the combined values back into dst: for every
// pointer-to-number field that accumulated at least one value, the
// average or maximum replaces the (never set) placeholder.
func finalize(v reflect.Value, path string, acc map[string]*numAgg, mode combineMode) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				finalize(v.Field(i), path+"/"+v.Type().Field(i).Name, acc, mode)
			}
		}
	case reflect.Ptr:
		if isNumberKind(v.Type().Elem().Kind()) {
			a := acc[path]
			if a == nil || a.count == 0 {
				return
			}
			val := a.max
			if mode == combineAvg {
				val = a.sum / float64(a.count)
			}
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			switch v.Elem().Kind() {
			case reflect.Float32, reflect.Float64:
				v.Elem().SetFloat(val)
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				v.Elem().SetInt(int64(val))
			default:
				v.Elem().SetUint(uint64(val))
			}
			return
		}
		if !v.IsNil() {
			finalize(v.Elem(), path, acc, mode)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			key, ok := elemKey(v.Index(i))
			if !ok {
				key = strconv.Itoa(i)
			}
			finalize(v.Index(i), path+"["+key+"]", acc, mode)
		}
	}
}

// deepCopyValue duplicates a struct value so the combined result never
// aliases the samples' storage.
func deepCopyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				out.Field(i).Set(deepCopyValue(v.Field(i)))
			}
		}
		return out
	case reflect.Ptr:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(deepCopyValue(v.Elem()))
		return out
	case reflect.Slice:
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return out
	default:
		return v
	}
}

// elemKey returns the identity of a GPUSample (Index) or NVMESample
// (Name), used to pair devices across samples.
func elemKey(v reflect.Value) (string, bool) {
	t := v.Type()
	if f, ok := t.FieldByName("Index"); ok && f.Type.Kind() == reflect.Int {
		return strconv.Itoa(int(v.Field(f.Index[0]).Int())), true
	}
	if f, ok := t.FieldByName("Name"); ok && f.Type.Kind() == reflect.String {
		return v.Field(f.Index[0]).String(), true
	}
	return "", false
}

func isNumberKind(k reflect.Kind) bool {
	switch k {
	case reflect.Float32, reflect.Float64, reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

// statsReader loads samples from the JSONL log files, caching a file's
// decoded samples until its size or modification time changes. A
// 5-second cadence is ~17k lines per day, so a cold week-sized scan
// is a few hundred milliseconds; the cache keeps repeated endpoint
// polls off the disk.
type statsReader struct {
	log   *slog.Logger
	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	modTime time.Time
	size    int64
	samples []Sample
}

func newStatsReader(log *slog.Logger) *statsReader {
	return &statsReader{log: log, cache: map[string]cacheEntry{}}
}

// readSamples returns the sample records of one file. A missing or
// unreadable file is no samples, not an error — the day may simply not
// have started.
func (r *statsReader) readSamples(path string) []Sample {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cache[path]; ok && c.size == st.Size() && c.modTime.Equal(st.ModTime()) {
		return c.samples
	}
	samples, err := decodeSamples(path)
	if err != nil {
		r.log.Warn("monitor: log file partially unreadable", "file", path, "err", err)
	}
	r.cache[path] = cacheEntry{modTime: st.ModTime(), size: st.Size(), samples: samples}
	return samples
}

// readAll returns the samples of all files, in file order.
func (r *statsReader) readAll(paths []string) []Sample {
	var out []Sample
	for _, p := range paths {
		out = append(out, r.readSamples(p)...)
	}
	return out
}

// decodeSamples parses one JSONL file, keeping only periodic sample
// records (model load/unload events are not part of the statistics).
// A truncated final line — the machine died mid-write — is skipped.
func decodeSamples(path string) ([]Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var s Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			continue // truncated or non-sample line
		}
		if s.Type == SampleType {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}
