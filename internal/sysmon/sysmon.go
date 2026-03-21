package sysmon

import (
	"fmt"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

// Snapshot contains current system resource usage.
type Snapshot struct {
	CPUPercent  float64
	RAMUsed     uint64
	RAMTotal    uint64
	RAMPercent  float64
	DiskUsed    uint64
	DiskTotal   uint64
	DiskPercent float64
}

// Collect gathers CPU, RAM, and root disk metrics.
func Collect() (*Snapshot, error) {
	cpuUsage, err := cpu.Percent(0, false)
	if err != nil {
		return nil, fmt.Errorf("failed to read CPU usage: %w", err)
	}
	if len(cpuUsage) == 0 {
		return nil, fmt.Errorf("failed to read CPU usage: no values returned")
	}

	vm, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("failed to read memory usage: %w", err)
	}

	rootDisk, err := disk.Usage("/")
	if err != nil {
		return nil, fmt.Errorf("failed to read disk usage: %w", err)
	}

	return &Snapshot{
		CPUPercent:  cpuUsage[0],
		RAMUsed:     vm.Used,
		RAMTotal:    vm.Total,
		RAMPercent:  vm.UsedPercent,
		DiskUsed:    rootDisk.Used,
		DiskTotal:   rootDisk.Total,
		DiskPercent: rootDisk.UsedPercent,
	}, nil
}
