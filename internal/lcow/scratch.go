package lcow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Microsoft/go-winio/vhd"
	"github.com/Microsoft/hcsshim/internal/copyfile"
	"github.com/Microsoft/hcsshim/internal/cow"
	"github.com/Microsoft/hcsshim/internal/hcsoci"
	"github.com/Microsoft/hcsshim/internal/timeout"
	"github.com/Microsoft/hcsshim/internal/uvm"
	"github.com/sirupsen/logrus"
)

// CreateScratch uses a utility VM to create an empty scratch disk of a
// requested size. It has a caching capability. If the cacheFile exists, and the
// request is for a default size, a copy of that is made to the target. If the
// size is non-default, or the cache file does not exist, it uses a utility VM
// to create target. It is the responsibility of the caller to synchronise
// simultaneous attempts to create the cache file.
func CreateScratch(lcowUVM *uvm.UtilityVM, destFile string, sizeGB uint32, cacheFile string) error {
	if lcowUVM == nil {
		return fmt.Errorf("no uvm")
	}

	if lcowUVM.OS() != "linux" {
		return errors.New("lcow::CreateScratch requires a linux utility VM to operate")
	}

	logrus.WithFields(logrus.Fields{
		"dest":   destFile,
		"sizeGB": sizeGB,
		"cache":  cacheFile,
	}).Debug("lcow::CreateScratch opts")

	// Retrieve from cache if the default size and already on disk
	if cacheFile != "" && sizeGB == DefaultScratchSizeGB {
		if _, err := os.Stat(cacheFile); err == nil {
			if err := copyfile.CopyFile(cacheFile, destFile, false); err != nil {
				return fmt.Errorf("failed to copy cached file '%s' to '%s': %s", cacheFile, destFile, err)
			}
			logrus.WithFields(logrus.Fields{
				"dest":  destFile,
				"cache": cacheFile,
			}).Debug("lcow::CreateScratch copied from cache")
			return nil
		}
	}

	// Create the VHDX
	if err := vhd.CreateVhdx(destFile, sizeGB, defaultVhdxBlockSizeMB); err != nil {
		return fmt.Errorf("failed to create VHDx %s: %s", destFile, err)
	}

	controller, lun, err := lcowUVM.AddSCSI(destFile, "", false) // No destination as not formatted
	if err != nil {
		return err
	}
	removeSCSI := true
	defer func() {
		if removeSCSI {
			lcowUVM.RemoveSCSI(destFile)
		}
	}()

	logrus.WithFields(logrus.Fields{
		"dest":       destFile,
		"controller": controller,
		"lun":        lun,
	}).Debug("lcow::CreateScratch device attached")

	// Validate /sys/bus/scsi/devices/C:0:0:L exists as a directory
	devicePath := fmt.Sprintf("/sys/bus/scsi/devices/%d:0:0:%d/block", controller, lun)
	testdCtx, cancel := context.WithTimeout(context.TODO(), timeout.TestDRetryLoop)
	defer cancel()
	for {
		cmd := hcsoci.CommandContext(testdCtx, lcowUVM, "test", "-d", devicePath)
		err := cmd.Run()
		if err == nil {
			break
		}
		if _, ok := err.(*hcsoci.ExitError); !ok {
			return fmt.Errorf("failed to run %+v following hot-add %s to utility VM: %s", cmd.Spec.Args, destFile, err)
		}
		time.Sleep(time.Millisecond * 10)
	}
	cancel()

	// Get the device from under the block subdirectory by doing a simple ls. This will come back as (eg) `sda`
	lsCtx, cancel := context.WithTimeout(context.TODO(), timeout.ExternalCommandToStart)
	cmd := hcsoci.CommandContext(lsCtx, lcowUVM, "ls", devicePath)
	lsOutput, err := cmd.Output()
	cancel()
	if err != nil {
		return fmt.Errorf("failed to `%+v` following hot-add %s to utility VM: %s", cmd.Spec.Args, destFile, err)
	}
	device := fmt.Sprintf(`/dev/%s`, bytes.TrimSpace(lsOutput))
	logrus.WithFields(logrus.Fields{
		"dest":   destFile,
		"device": device,
	}).Debug("lcow::CreateScratch device guest location")

	// Format it ext4
	mkfsCtx, cancel := context.WithTimeout(context.TODO(), timeout.ExternalCommandToStart)
	cmd = hcsoci.CommandContext(mkfsCtx, lcowUVM, "mkfs.ext4", "-q", "-E", "lazy_itable_init=0,nodiscard", "-O", `^has_journal,sparse_super2,^resize_inode`, device)
	var mkfsStderr bytes.Buffer
	cmd.Stderr = &mkfsStderr
	err = cmd.Run()
	cancel()
	if err != nil {
		return fmt.Errorf("failed to `%+v` following hot-add %s to utility VM: %s", cmd.Spec.Args, destFile, err)
	}

	// Hot-Remove before we copy it
	removeSCSI = false
	if err := lcowUVM.RemoveSCSI(destFile); err != nil {
		return fmt.Errorf("failed to hot-remove: %s", err)
	}

	// Populate the cache.
	if cacheFile != "" && (sizeGB == DefaultScratchSizeGB) {
		if err := copyfile.CopyFile(destFile, cacheFile, true); err != nil {
			return fmt.Errorf("failed to seed cache '%s' from '%s': %s", destFile, cacheFile, err)
		}
	}

	logrus.WithField("dest", destFile).Debug("lcow::CreateScratch created (non-cache)")
	return nil
}

func waitForProcess(p cow.Process) (int, error) {
	ch := make(chan error, 1)
	go func() {
		ch <- p.Wait()
	}()

	t := time.NewTimer(timeout.ExternalCommandToComplete)
	select {
	case <-ch:
		t.Stop()
	case <-t.C:
		p.Kill()
	}
	return p.ExitCode()
}
