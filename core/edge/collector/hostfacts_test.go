package collector

import (
	"context"
	"testing"

	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Two device columns (os_version, disk_total_bytes) were permanently zero
// because the wire struct had no column to carry them, and nothing in this
// package noticed: HostInfo had no test at all. These cases compare each
// collector against gopsutil rather than against a literal, so they pin the
// wiring without depending on what the machine running them happens to
// report — a host with no resolvable platform version or an unstattable
// root passes here exactly as it does in production.

func TestRootDiskTotalBytesIsTheRootCapacity(t *testing.T) {
	want, err := disk.UsageWithContext(context.Background(), "/")
	if err != nil {
		t.Skipf("root filesystem not statable here: %v", err)
	}
	if got := rootDiskTotalBytes(context.Background()); got != want.Total {
		t.Errorf("rootDiskTotalBytes() = %d, want the total of / = %d", got, want.Total)
	}
}

func TestCollectorsReportPlatformVersionAndRootCapacity(t *testing.T) {
	ctx := context.Background()
	info, err := host.InfoWithContext(ctx)
	if err != nil {
		t.Skipf("host info unavailable here: %v", err)
	}
	wantDisk := rootDiskTotalBytes(ctx)

	embedded, err := (&EmbeddedCollector{}).HostInfo(ctx)
	if err != nil {
		t.Fatalf("embedded HostInfo: %v", err)
	}
	scraper := &Scraper{}
	scraped, err := scraper.HostInfo(ctx)
	if err != nil {
		t.Fatalf("scrape HostInfo: %v", err)
	}

	for name, got := range map[string]tunnel.HostInfo{"embedded": embedded, "scrape": scraped} {
		if got.OSVersion != info.PlatformVersion {
			t.Errorf("%s collector OSVersion = %q, want gopsutil's PlatformVersion %q",
				name, got.OSVersion, info.PlatformVersion)
		}
		if got.DiskTotalBytes != wantDisk {
			t.Errorf("%s collector DiskTotalBytes = %d, want the root capacity %d",
				name, got.DiskTotalBytes, wantDisk)
		}
	}
}
