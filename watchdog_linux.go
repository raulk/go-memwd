package watchdog

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/containerd/cgroups/v3/cgroup2"
)

var (
	pid = os.Getpid()
	//memSubsystem = cgroups.SingleSubsystem(cgroups.V1, cgroups.Memory)
)

// CgroupDriven initializes a cgroups-driven watchdog. It will try to discover
// the memory limit from the cgroup of the process (derived from /proc/self/cgroup),
// or from the root cgroup path if the PID == 1 (which indicates that the process
// is running in a container).
//
// Memory usage is calculated by querying the cgroup stats.
//
// This function will return an error immediately if the OS does not support cgroups,
// or if another error occurs during initialization. The caller can then safely fall
// back to the system driven watchdog.
func CgroupDriven(frequency time.Duration, policyCtor PolicyCtor) (err error, stopFn func()) {
	switch cgroups.Mode() {
	case cgroups.Unified:
		return cgroupv2Driven(frequency, policyCtor)
	case cgroups.Legacy:
		return cgroupv1Driven(frequency, policyCtor)
	case cgroups.Unavailable:
		fallthrough
	default:
		return fmt.Errorf("Cgroups not supported in this environment"), func() {}
	}
}

func cgroupv1Driven(frequency time.Duration, policyCtor PolicyCtor) (err error, stopFn func()) {
	// use self path unless our PID is 1, in which case we're running inside
	// a container and our limits are in the root path.
	path := cgroup1.NestedPath("")
	if pid := os.Getpid(); pid == 1 {
		path = cgroup1.RootPath
	}

	cgroup, err := cgroup1.Load(path, cgroup1.WithHiearchy(func() ([]cgroup1.Subsystem, error) {
		system, err := cgroup1.Default()
		if err != nil {
			return nil, err
		}
		out := []cgroup1.Subsystem{}
		for _, v := range system {
			switch v.Name() {
			case cgroup1.Memory:
				out = append(out, v)
			}
		}
		return out, nil
	}))
	if err != nil {
		return fmt.Errorf("failed to load cgroup1 for process: %w", err), nil
	}

	var limit uint64
	if stat, err := cgroup.Stat(); err != nil {
		return fmt.Errorf("failed to load memory cgroup1 stats: %w", err), nil
	} else if stat.Memory == nil || stat.Memory.Usage == nil {
		return fmt.Errorf("cgroup1 memory stats are nil; aborting"), nil
	} else {
		log.Printf("stat: %v", stat.Memory.Usage)
		limit = stat.Memory.Usage.Limit
	}

	if limit == 0 {
		return fmt.Errorf("cgroup1 limit is 0; refusing to start memory watchdog"), nil
	}

	policy, err := policyCtor(limit)
	if err != nil {
		return fmt.Errorf("failed to construct policy with limit %d: %w", limit, err), nil
	}

	if err := start(UtilizationProcess); err != nil {
		return err, nil
	}

	_watchdog.wg.Add(1)
	go pollingWatchdog(policy, frequency, limit, func() (uint64, error) {
		stat, err := cgroup.Stat()
		if err != nil {
			return 0, err
		} else if stat.Memory == nil || stat.Memory.Usage == nil {
			return 0, fmt.Errorf("cgroup1 memory stats are nil; aborting")
		}
		return stat.Memory.Usage.Usage, nil
	})

	return nil, stop
}
func cgroupv2Driven(frequency time.Duration, policyCtor PolicyCtor) (err error, stopFn func()) {
	// use self path unless our PID is 1, in which case we're running inside
	// a container and our limits are in the root path.

	pid := os.Getpid()
	path, err := cgroup2.PidGroupPath(pid)
	if err != nil {
		return fmt.Errorf("failed to load cgroup2 path for process pid %d: %w", pid, err), nil
	}

	cgroup, err := cgroup2.Load(path)
	if err != nil {
		return fmt.Errorf("failed to load cgroup2 for process: %w", err), nil
	}

	var limit uint64
	if stat, err := cgroup.Stat(); err != nil {
		return fmt.Errorf("failed to load cgroup2 memory stats: %w", err), nil
	} else if stat.Memory == nil {
		return fmt.Errorf("cgroup2 memory stats are nil; aborting"), nil
	} else {
		limit = stat.Memory.UsageLimit
	}

	if limit == 0 {
		return fmt.Errorf("cgroup2 limit is 0; refusing to start memory watchdog"), nil
	}

	policy, err := policyCtor(limit)
	if err != nil {
		return fmt.Errorf("failed to construct policy with limit %d: %w", limit, err), nil
	}

	if err := start(UtilizationProcess); err != nil {
		return err, nil
	}

	_watchdog.wg.Add(1)
	go pollingWatchdog(policy, frequency, limit, func() (uint64, error) {
		stat, err := cgroup.Stat()
		if err != nil {
			return 0, err
		} else if stat.Memory == nil {
			return 0, fmt.Errorf("cgroup2 memory stats are nil; aborting")
		}
		return stat.Memory.Usage, nil
	})

	return nil, stop
}
