package storage

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ValidateFilesystem 按最长路径匹配找到所在挂载点，并检查挂载标志和超级块标志。
// 只接受具有 SQLite 锁语义的已知本地文件系统；
// 挂载表不可读或格式错误时拒绝打开。
func ValidateFilesystem(path string, mountInfo io.Reader) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("local SQLite filesystem validation requires Linux")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if mountInfo == nil {
		return fmt.Errorf("mount information is required")
	}
	longest := -1
	var filesystem, mountOptions, superOptions string
	scanner := bufio.NewScanner(mountInfo)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || len(fields) != separator+4 {
			return fmt.Errorf("malformed mount information")
		}
		for _, field := range fields[:2] {
			if _, err := strconv.ParseUint(field, 10, 64); err != nil {
				return fmt.Errorf("malformed mount identifier: %w", err)
			}
		}
		device := strings.Split(fields[2], ":")
		if len(device) != 2 {
			return fmt.Errorf("malformed mount device")
		}
		for _, field := range device {
			if _, err := strconv.ParseUint(field, 10, 64); err != nil {
				return fmt.Errorf("malformed mount device: %w", err)
			}
		}
		// 某些内核伪文件系统暴露非路径形式的根（例如命名空间标签）。
		// 决定某个条目是否包含数据库路径的是挂载点，而不是这个诊断用根。
		if _, err := unescapeMountPath(fields[3]); err != nil {
			return fmt.Errorf("malformed mount root %q", fields[3])
		}
		mount, err := unescapeMountPath(fields[4])
		if err != nil || !filepath.IsAbs(mount) || filepath.Clean(mount) != mount {
			return fmt.Errorf("malformed mount path %q", fields[4])
		}
		if _, err := unescapeMountPath(fields[separator+2]); err != nil {
			return err
		}
		if (mount == "/" || absolute == mount || strings.HasPrefix(absolute, mount+string(filepath.Separator))) && len(mount) >= longest {
			longest = len(mount)
			filesystem = fields[separator+1]
			mountOptions = fields[5]
			superOptions = fields[separator+3]
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read mount information: %w", err)
	}
	if longest < 0 {
		return fmt.Errorf("no containing mount found for %q", absolute)
	}
	for _, options := range []string{mountOptions, superOptions} {
		writable := false
		for _, option := range strings.Split(options, ",") {
			if option == "ro" {
				return fmt.Errorf("database filesystem is read-only")
			}
			writable = writable || option == "rw"
		}
		if !writable {
			return fmt.Errorf("database filesystem has no writable mount declaration")
		}
	}
	switch filesystem {
	case "ext2", "ext3", "ext4", "xfs", "btrfs", "f2fs", "zfs", "tmpfs", "ramfs", "overlay", "aufs", "ubifs", "jffs2":
		return nil
	default:
		return fmt.Errorf("database filesystem %q is remote or unsupported", filesystem)
	}
}

func unescapeMountPath(path string) (string, error) {
	if !strings.Contains(path, `\`) {
		return path, nil
	}
	var decoded strings.Builder
	decoded.Grow(len(path))
	for i := 0; i < len(path); i++ {
		if path[i] != '\\' {
			decoded.WriteByte(path[i])
			continue
		}
		if i+3 >= len(path) {
			return "", fmt.Errorf("incomplete mount path escape")
		}
		switch path[i+1 : i+4] {
		case "040":
			decoded.WriteByte(' ')
		case "011":
			decoded.WriteByte('\t')
		case "012":
			decoded.WriteByte('\n')
		case "134":
			decoded.WriteByte('\\')
		default:
			return "", fmt.Errorf("invalid mount path escape")
		}
		i += 3
	}
	return decoded.String(), nil
}

func validateLocalFilesystem(path string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("local SQLite filesystem validation requires Linux")
	}
	mountInfo, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("open mount information: %w", err)
	}
	defer mountInfo.Close()
	return ValidateFilesystem(path, mountInfo)
}
