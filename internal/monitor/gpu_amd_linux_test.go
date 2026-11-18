//go:build linux

package monitor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeAMDTree builds a sysfs-like DRM tree with one AMD card and one
// non-AMD card, mirroring the layout gpu_amd_linux.go reads.
func fakeAMDTree(t *testing.T) (drmDir string) {
	t.Helper()
	root := t.TempDir()
	drm := filepath.Join(root, "drm")
	amd := filepath.Join(drm, "card0", "device")
	other := filepath.Join(drm, "card1", "device")
	for _, d := range []string{amd, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(amd, "vendor", "0x1002\n")
	write(amd, "device", "0x747e\n")
	write(other, "vendor", "0x10de\n") // an NVIDIA card must be ignored
	hw := filepath.Join(amd, "hwmon", "hwmon1")
	if err := os.MkdirAll(hw, 0o755); err != nil {
		t.Fatal(err)
	}
	write(hw, "temp1_input", "45000\n") // edge, millidegrees
	write(hw, "temp2_input", "52000\n") // junction
	write(hw, "temp3_input", "48000\n") // memory
	write(hw, "fan1_input", "1200\n")
	write(hw, "power1_average", "230000000\n") // 230 W
	write(amd, "gpu_busy_percent", "97\n")
	write(amd, "mem_info_vram_used", strconv.FormatInt(16*(1<<20), 10)+"\n")
	write(amd, "mem_info_vram_total", strconv.FormatInt(16*(1<<20), 10)+"\n")
	write(amd, "product_name", "AMD Radeon RX 7800 XT\n")
	write(amd, "pp_dpm_sclk", "0: 500Mhz\n1: 2254Mhz *\n2: 2600Mhz\n")
	write(amd, "pp_dpm_mclk", "0: 96Mhz *\n1: 1218Mhz\n")
	return drm
}

func TestAMDGPUsFromFakeSysfs(t *testing.T) {
	drm := fakeAMDTree(t)
	cards, err := filepath.Glob(filepath.Join(drm, "card[0-9]*"))
	if err != nil || len(cards) != 2 {
		t.Fatalf("glob = %v, %v", cards, err)
	}
	// Reproduce the construction against the fake tree.
	dev := filepath.Join(drm, "card0", "device")
	c := amdCard{index: 0, device: dev, hwmon: filepath.Join(dev, "hwmon", "hwmon1"), name: "AMD Radeon RX 7800 XT"}
	s := c.sample()

	if s.TempC == nil || *s.TempC != 45 {
		t.Errorf("TempC = %v, want 45", s.TempC)
	}
	if s.JunctionTempC == nil || *s.JunctionTempC != 52 {
		t.Errorf("JunctionTempC = %v, want 52", s.JunctionTempC)
	}
	if s.MemTempC == nil || *s.MemTempC != 48 {
		t.Errorf("MemTempC = %v, want 48", s.MemTempC)
	}
	if s.FanRPM == nil || *s.FanRPM != 1200 {
		t.Errorf("FanRPM = %v, want 1200", s.FanRPM)
	}
	if s.PowerW == nil || *s.PowerW != 230 {
		t.Errorf("PowerW = %v, want 230", s.PowerW)
	}
	if s.UtilPct == nil || *s.UtilPct != 97 {
		t.Errorf("UtilPct = %v, want 97", s.UtilPct)
	}
	if s.VRAMUsedMiB == nil || *s.VRAMUsedMiB != 16 {
		t.Errorf("VRAMUsedMiB = %v, want 16", s.VRAMUsedMiB)
	}
	// sclk: current 2254 (the '*' level), max 2600 (the last level).
	if s.SMClkMHz == nil || *s.SMClkMHz != 2254 || s.SMClkMaxMHz == nil || *s.SMClkMaxMHz != 2600 {
		t.Errorf("SMClk = %v/%v, want 2254/2600", s.SMClkMHz, s.SMClkMaxMHz)
	}
	if s.MemClkMHz == nil || *s.MemClkMHz != 96 || s.MemClkMaxMHz == nil || *s.MemClkMaxMHz != 1218 {
		t.Errorf("MemClk = %v/%v, want 96/1218", s.MemClkMHz, s.MemClkMaxMHz)
	}
}

func TestAMDCardCanaryOmitsEverything(t *testing.T) {
	// A dead SMU fails the canary read (temp1_input): every GPU field
	// must be omitted rather than half-read.
	dev := t.TempDir()
	c := amdCard{index: 0, device: dev, hwmon: dev, name: "x"}
	s := c.sample()
	if s.TempC != nil || s.UtilPct != nil || s.PowerW != nil {
		t.Errorf("sample = %+v, want all GPU fields omitted", s)
	}
}

func TestAMDCardName(t *testing.T) {
	dev := fakeAMDTree(t)
	amd := filepath.Join(dev, "card0", "device")
	// The fake tree carries a product_name; it wins.
	if n := amdCardName(amd, 0, nil); n != "AMD Radeon RX 7800 XT" {
		t.Errorf("name = %q, want the driver product name", n)
	}
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// No product name: the pci.ids database, then the cardN [id]
	// fallback — deterministic regardless of the host's own pci.ids.
	write(amd, "product_name", "")
	db := map[string]string{"747e": "Navi 32 [Radeon RX 7700 XT / 7800 XT]"}
	if n := amdCardName(amd, 0, db); n != "Navi 32 [Radeon RX 7700 XT / 7800 XT]" {
		t.Errorf("name = %q, want the pci.ids entry", n)
	}
	if n := amdCardName(amd, 0, nil); n != "card0 [747e]" {
		t.Errorf("name = %q, want the cardN [pci id] fallback", n)
	}
	// The database parser itself: vendor section, subsystem lines,
	// comments, other vendors.
	parsed := parsePCIIDs(strings.NewReader("# comment\n\n1002  AMD\n\t747e  Navi 32 [Radeon RX 7700 XT / 7800 XT]\n\t\t747e  subsystem noise\n1234  other\n\t0001  x\n"), "1002")
	if got := parsed["747e"]; got != "Navi 32 [Radeon RX 7700 XT / 7800 XT]" {
		t.Errorf("pci.ids lookup = %q", got)
	}
	if _, ok := parsed["0001"]; ok {
		t.Error("device of another vendor leaked into the map")
	}
	if parsed := parsePCIIDs(strings.NewReader("1002  AMD\n"), "9999"); parsed != nil {
		t.Errorf("parsePCIIDs(unknown vendor) = %v, want nil", parsed)
	}
}

func TestReadDiskstatsAndNVMePairing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "diskstats")
	content := "   1       0 nvme0n1 123 4 500000 10 20 6 300000 5 0 0 0\n" +
		" 259       1 nvme0n1p1 5 0 100 1 2 50 0 0 0 0\n" +
		" 259       0 nvme0 3 0 0 0 0 0 0 0 0 0\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	counters, err := readDiskstats(path)
	if err != nil {
		t.Fatal(err)
	}
	want := [2]int64{500000 * 512, 300000 * 512}
	if got := counters["nvme0n1"]; got != want {
		t.Errorf("nvme0n1 = %v, want %v", got, want)
	}
	if _, ok := counters["nvme0n1p1"]; !ok {
		t.Error("partition line missing (kept, filtered later)")
	}

	// The disk<->controller pairing: nvme0n1 -> nvme0.
	if c := nvmeController("nvme0n1"); c != "nvme0" {
		t.Errorf("controller = %q, want nvme0", c)
	}
}
