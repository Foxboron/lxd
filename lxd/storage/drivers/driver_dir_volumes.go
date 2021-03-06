package drivers

import (
	"fmt"
	"io"
	"os"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/rsync"
	"github.com/lxc/lxd/lxd/storage/quota"
	"github.com/lxc/lxd/shared"
	log "github.com/lxc/lxd/shared/log15"
)

// CreateVolume creates an empty volume and can optionally fill it by executing the supplied
// filler function.
func (d *dir) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	volPath := vol.MountPath()

	revert := revert.New()
	defer revert.Fail()

	// Create the volume itself.
	err := vol.EnsureMountPath()
	if err != nil {
		return err
	}
	revert.Add(func() { os.RemoveAll(volPath) })

	// Create sparse loopback file if volume is block.
	rootBlockPath := ""
	if vol.contentType == ContentTypeBlock {
		// We expect the filler to copy the VM image into this path.
		rootBlockPath, err = d.GetVolumeDiskPath(vol)
		if err != nil {
			return err
		}
	} else {
		revertFunc, err := d.setupInitialQuota(vol)
		if err != nil {
			return err
		}

		if revertFunc != nil {
			revert.Add(revertFunc)
		}
	}

	// Run the volume filler function if supplied.
	if filler != nil && filler.Fill != nil {
		d.logger.Debug("Running filler function", log.Ctx{"path": volPath})
		err = filler.Fill(volPath, rootBlockPath)
		if err != nil {
			return err
		}
	}

	// If we are creating a block volume, resize it to the requested size or the default.
	// We expect the filler function to have converted the qcow2 image to raw into the rootBlockPath.
	if vol.contentType == ContentTypeBlock {
		err := ensureVolumeBlockFile(vol, rootBlockPath)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// CreateVolumeFromBackup restores a backup tarball onto the storage device.
func (d *dir) CreateVolumeFromBackup(vol Volume, snapshots []string, srcData io.ReadSeeker, optimizedStorage bool, op *operations.Operation) (func(vol Volume) error, func(), error) {
	// Run the generic backup unpacker
	postHook, revertHook, err := genericBackupUnpack(d.withoutGetVolID(), vol, snapshots, srcData, op)
	if err != nil {
		return nil, nil, err
	}

	// Define a post hook function that can be run once the backup config has been restored.
	// This will setup the quota using the restored config.
	postHookWrapper := func(vol Volume) error {
		if postHook != nil {
			err := postHook(vol)
			if err != nil {
				return err
			}
		}

		_, err := d.setupInitialQuota(vol)
		if err != nil {
			return err
		}

		return nil
	}

	return postHookWrapper, revertHook, nil
}

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *dir) CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, op *operations.Operation) error {
	var err error
	var srcSnapshots []Volume

	if copySnapshots && !srcVol.IsSnapshot() {
		// Get the list of snapshots from the source.
		srcSnapshots, err = srcVol.Snapshots(op)
		if err != nil {
			return err
		}
	}

	// Run the generic copy.
	return genericCopyVolume(d, d.setupInitialQuota, vol, srcVol, srcSnapshots, false, op)
}

// CreateVolumeFromMigration creates a volume being sent via a migration.
func (d *dir) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	if vol.contentType != ContentTypeFS {
		return ErrNotSupported
	}

	if volTargetArgs.MigrationType.FSType != migration.MigrationFSType_RSYNC {
		return ErrNotSupported
	}

	return genericCreateVolumeFromMigration(d, d.setupInitialQuota, vol, conn, volTargetArgs, preFiller, op)
}

// RefreshVolume provides same-pool volume and specific snapshots syncing functionality.
func (d *dir) RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, op *operations.Operation) error {
	return genericCopyVolume(d, d.setupInitialQuota, vol, srcVol, srcSnapshots, true, op)
}

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then
// this function will return an error.
func (d *dir) DeleteVolume(vol Volume, op *operations.Operation) error {
	snapshots, err := d.VolumeSnapshots(vol, op)
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		return fmt.Errorf("Cannot remove a volume that has snapshots")
	}

	volPath := vol.MountPath()

	// If the volume doesn't exist, then nothing more to do.
	if !shared.PathExists(volPath) {
		return nil
	}

	// Get the volume ID for the volume, which is used to remove project quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return err
	}

	// Remove the project quota.
	err = d.deleteQuota(volPath, volID)
	if err != nil {
		return err
	}

	// Remove the volume from the storage device.
	err = os.RemoveAll(volPath)
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "Failed to remove '%s'", volPath)
	}

	// Although the volume snapshot directory should already be removed, lets remove it here
	// to just in case the top-level directory is left.
	err = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, vol.name)
	if err != nil {
		return err
	}

	return nil
}

// HasVolume indicates whether a specific volume exists on the storage pool.
func (d *dir) HasVolume(vol Volume) bool {
	return genericVFSHasVolume(vol)
}

// ValidateVolume validates the supplied volume config. Optionally removes invalid keys from the volume's config.
func (d *dir) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	return d.validateVolume(vol, nil, removeUnknownKeys)
}

// UpdateVolume applies config changes to the volume.
func (d *dir) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	if vol.contentType != ContentTypeFS {
		return ErrNotSupported
	}

	if _, changed := changedConfig["size"]; changed {
		err := d.SetVolumeQuota(vol, changedConfig["size"], nil)
		if err != nil {
			return err
		}
	}

	return nil
}

// GetVolumeUsage returns the disk space used by the volume.
func (d *dir) GetVolumeUsage(vol Volume) (int64, error) {
	volPath := vol.MountPath()
	ok, err := quota.Supported(volPath)
	if err != nil || !ok {
		return 0, nil
	}

	// Get the volume ID for the volume to access quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return -1, err
	}

	projectID := d.quotaProjectID(volID)

	// Get project quota used.
	size, err := quota.GetProjectUsage(volPath, projectID)
	if err != nil {
		return -1, err
	}

	return size, nil
}

// SetVolumeQuota sets the quota on the volume.
func (d *dir) SetVolumeQuota(vol Volume, size string, op *operations.Operation) error {
	volPath := vol.MountPath()

	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return err
	}

	return d.setQuota(volPath, volID, size)
}

// GetVolumeDiskPath returns the location of a disk volume.
func (d *dir) GetVolumeDiskPath(vol Volume) (string, error) {
	return genericVFSGetVolumeDiskPath(vol)
}

// MountVolume simulates mounting a volume. As dir driver doesn't have volumes to mount it returns
// false indicating that there is no need to issue an unmount.
func (d *dir) MountVolume(vol Volume, op *operations.Operation) (bool, error) {
	return false, nil
}

// UnmountVolume simulates unmounting a volume. As dir driver doesn't have volumes to unmount it
// returns false indicating the volume was already unmounted.
func (d *dir) UnmountVolume(vol Volume, op *operations.Operation) (bool, error) {
	return false, nil
}

// RenameVolume renames a volume and its snapshots.
func (d *dir) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	return genericVFSRenameVolume(d, vol, newVolName, op)
}

// MigrateVolume sends a volume for migration.
func (d *dir) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, op *operations.Operation) error {
	if vol.contentType != ContentTypeFS {
		return ErrNotSupported
	}

	if volSrcArgs.MigrationType.FSType != migration.MigrationFSType_RSYNC {
		return ErrNotSupported
	}

	return genericVFSMigrateVolume(d, d.state, vol, conn, volSrcArgs, op)
}

// BackupVolume copies a volume (and optionally its snapshots) to a specified target path.
// This driver does not support optimized backups.
func (d *dir) BackupVolume(vol Volume, targetPath string, optimized bool, snapshots bool, op *operations.Operation) error {
	return genericVFSBackupVolume(d, vol, targetPath, snapshots, op)
}

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *dir) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	srcPath := GetVolumeMountPath(d.name, snapVol.volType, parentName)
	snapPath := snapVol.MountPath()

	// Create the parent directory.
	err := createParentSnapshotDirIfMissing(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	// Create snapshot directory.
	err = snapVol.EnsureMountPath()
	if err != nil {
		return err
	}

	revertPath := true
	defer func() {
		if revertPath {
			os.RemoveAll(snapPath)
		}
	}()

	bwlimit := d.config["rsync.bwlimit"]

	// Copy volume into snapshot directory.
	_, err = rsync.LocalCopy(srcPath, snapPath, bwlimit, true)
	if err != nil {
		return err
	}

	revertPath = false
	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device. The volName and snapshotName
// must be bare names and should not be in the format "volume/snapshot".
func (d *dir) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	snapPath := snapVol.MountPath()

	// Remove the snapshot from the storage device.
	err := os.RemoveAll(snapPath)
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "Failed to remove '%s'", snapPath)
	}

	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)

	// Remove the parent snapshot directory if this is the last snapshot being removed.
	err = deleteParentSnapshotDirIfEmpty(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	return nil
}

// MountVolumeSnapshot sets up a read-only mount on top of the snapshot to avoid accidental modifications.
func (d *dir) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	snapPath := snapVol.MountPath()
	return mountReadOnly(snapPath, snapPath)
}

// UnmountVolumeSnapshot removes the read-only mount placed on top of a snapshot.
func (d *dir) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	snapPath := snapVol.MountPath()
	return forceUnmount(snapPath)
}

// VolumeSnapshots returns a list of snapshots for the volume.
func (d *dir) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	return genericVFSVolumeSnapshots(d, vol, op)
}

// RestoreVolume restores a volume from a snapshot.
func (d *dir) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	srcPath := GetVolumeMountPath(d.name, vol.volType, GetSnapshotVolumeName(vol.name, snapshotName))
	if !shared.PathExists(srcPath) {
		return fmt.Errorf("Snapshot not found")
	}

	volPath := vol.MountPath()

	// Restore using rsync.
	bwlimit := d.config["rsync.bwlimit"]
	_, err := rsync.LocalCopy(srcPath, volPath, bwlimit, true)
	if err != nil {
		return errors.Wrap(err, "Failed to rsync volume")
	}

	return nil
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *dir) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	return genericVFSRenameVolumeSnapshot(d, snapVol, newSnapshotName, op)
}
