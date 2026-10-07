package collector

import (
	"context"

	"github.com/shirou/gopsutil/v3/disk"
)

// rootDiskTotalBytes reports the capacity of the filesystem the host boots
// from, in bytes.
//
// It is a capacity fact rather than a gauge: the free/used percentages arrive
// on the metric path and are refreshed every scrape, while "total" changes
// only when someone repartitions a disk. The cloud stores it once per
// register as a device fact so the device list can render "used / total"
// without joining host_metrics.
//
// Best-effort by construction: 0 when the platform cannot stat the root
// mount (a Windows host without a drive letter mapped to "/", a locked
// container root). 0 is the column's zero value, so the cloud stores it as
// unknown rather than as a real capacity — which is why the wire column is
// omitempty and no caller treats 0 as authoritative.
func rootDiskTotalBytes(ctx context.Context) uint64 {
	u, err := disk.UsageWithContext(ctx, "/")
	if err != nil || u == nil {
		return 0
	}
	return u.Total
}
