package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
	"truenas/truenas_incus_ctl/core"
)

// Локалната страна на NVMe-oF: свързване, намиране на устройството, разкачане.
//
// Разликите спрямо iSCSI, всяка от които е причина този файл да съществува отделно:
//
//   - няма userspace демон. iSCSI иска `iscsid` да върви; при NVMe работата я вършат
//     модулите `nvme-tcp`/`nvme-fabrics` в ядрото.
//   - устройството НЕ се появява в /dev/disk/by-path с предвидимо име. Пътят се намира
//     през sysfs: /sys/class/nvme-subsystem/*/subsysnqn се сверява с NQN-а, а блоковото
//     устройство е дете на съвпадналия. Същата логика е в incus-nvme-initiator.sh и е
//     доказана по нодовете.
//   - разкачането е по NQN, не по портал+таргет.

const nvmeSubsystemsDir = "/sys/class/nvme-subsystem"

func CheckNvmeCliExists() error {
	if _, err := exec.LookPath("nvme"); err != nil {
		return fmt.Errorf("nvme-cli is not installed or not in PATH, install the \"nvme-cli\" package")
	}
	return nil
}

func requireRootForNvme(action string) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	if u.Uid != "0" {
		return fmt.Errorf("%s requires root privileges", action)
	}
	return nil
}

// FindNvmeDeviceBySubNqn връща /dev/nvmeXnY за даден subsystem NQN, или "" ако го няма.
func FindNvmeDeviceBySubNqn(subNqn string) string {
	subsystems, err := os.ReadDir(nvmeSubsystemsDir)
	if err != nil {
		return ""
	}

	for _, sub := range subsystems {
		nqnPath := filepath.Join(nvmeSubsystemsDir, sub.Name(), "subsysnqn")
		raw, err := os.ReadFile(nqnPath)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(raw)) != subNqn {
			continue
		}

		// Namespace-ите са деца на subsystem-а и се казват nvmeXnY.
		entries, err := os.ReadDir(filepath.Join(nvmeSubsystemsDir, sub.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "nvme") || !strings.Contains(name[4:], "n") {
				continue
			}
			devPath := "/dev/" + name
			if info, err := os.Stat(devPath); err == nil && info.Mode()&os.ModeDevice != 0 {
				return devPath
			}
		}
	}
	return ""
}

func RunNvmeConnect(addr string, port int, subNqn string) error {
	if err := CheckNvmeCliExists(); err != nil {
		return err
	}
	args := []string{
		"connect",
		"-t", "tcp",
		"-a", stripIpV6Brackets(addr),
		"-s", fmt.Sprint(port),
		"-n", subNqn,
	}
	out, err, status := core.RunCommand("nvme", args...)
	if err != nil {
		// Вече свързан subsystem не е грешка — командата е идемпотентна по замисъл.
		if strings.Contains(strings.ToLower(out), "already connected") {
			return nil
		}
		return fmt.Errorf("nvme connect failed (%d): %v", status, err)
	}
	return nil
}

func RunNvmeDisconnect(subNqn string) error {
	if err := CheckNvmeCliExists(); err != nil {
		return err
	}
	out, err, status := core.RunCommand("nvme", "disconnect", "-n", subNqn)
	if err != nil {
		lower := strings.ToLower(out)
		if strings.Contains(lower, "not found") || strings.Contains(lower, "no controllers") {
			return nil
		}
		return fmt.Errorf("nvme disconnect failed (%d): %v", status, err)
	}
	return nil
}

// WaitForNvmeDevice изчаква устройството да се появи след connect.
//
// Ядрото връща управлението преди namespace-ът да е разгледан, затова между успешен
// connect и наличен /dev/nvmeXnY минава време. Без изчакване следващата стъпка получава
// „няма такъв файл" и всичко изглежда като провалено свързване.
func WaitForNvmeDevice(subNqn string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		if dev := FindNvmeDeviceBySubNqn(subNqn); dev != "" {
			return dev
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// WaitForNvmeDeviceGone е обратното — за deactivate --wait.
func WaitForNvmeDeviceGone(subNqn string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if FindNvmeDeviceBySubNqn(subNqn) == "" {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}
