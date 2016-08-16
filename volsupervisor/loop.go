package volsupervisor

import (
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/contiv/volplugin/config"
	"github.com/contiv/volplugin/lock"
	"github.com/contiv/volplugin/storage"
	"github.com/contiv/volplugin/storage/backend"
)

var (
	volumes     = map[string]*config.Volume{}
	volumeMutex = &sync.Mutex{}
)

func (dc *DaemonConfig) pruneSnapshots(val *config.Volume) {
	logrus.Infof("starting snapshot prune for %q", val.VolumeName)
	if val.Backends.Snapshot == "" {
		logrus.Debugf("Snapshot driver for volume %v was empty, not snapshotting.", val)
		return
	}

	uc := &config.UseSnapshot{
		Volume: val.String(),
		Reason: lock.ReasonSnapshotPrune,
	}

	err := lock.NewDriver(dc.Config).ExecuteWithUseLock(uc, func(ld *lock.Driver, uc config.UseLocker) error {
		driver, err := backend.NewSnapshotDriver(val.Backends.Snapshot)
		if err != nil {
			logrus.Errorf("failed to get driver: %v", err)
			return err
		}

		driverOpts := storage.DriverOptions{
			Volume: storage.Volume{
				Name: val.String(),
				Params: storage.Params{
					"pool": val.DriverOptions["pool"],
				},
			},
			Timeout: dc.Global.Timeout,
		}

		list, err := driver.ListSnapshots(driverOpts)
		if err != nil {
			logrus.Errorf("Could not list snapshots for volume %q: %v", val.VolumeName, err)
			return err
		}

		logrus.Debugf("Volume %q: keeping %d snapshots", val, val.RuntimeOptions.Snapshot.Keep)

		toDeleteCount := len(list) - int(val.RuntimeOptions.Snapshot.Keep)
		if toDeleteCount < 0 {
			return nil
		}

		for i := 0; i < toDeleteCount; i++ {
			logrus.Infof("Removing snapshot %q for volume %q", list[i], val.VolumeName)
			if err := driver.RemoveSnapshot(list[i], driverOpts); err != nil {
				logrus.Errorf("Removing snapshot %q for volume %q failed: %v", list[i], val.VolumeName, err)
			}
		}

		return nil
	})

	if err != nil {
		logrus.Errorf("Error removing snapshot for volume %q: %v", val, err)
	}
}

func (dc *DaemonConfig) createSnapshot(val *config.Volume) {
	logrus.Infof("Snapshotting %q.", val)

	uc := &config.UseSnapshot{
		Volume: val.String(),
		Reason: lock.ReasonSnapshot,
	}

	err := lock.NewDriver(dc.Config).ExecuteWithUseLock(uc, func(ld *lock.Driver, uc config.UseLocker) error {
		driver, err := backend.NewSnapshotDriver(val.Backends.Snapshot)
		if err != nil {
			logrus.Errorf("Error establishing driver backend %q; cannot snapshot", val.Backends.Snapshot)
			return err
		}

		driverOpts := storage.DriverOptions{
			Volume: storage.Volume{
				Name: val.String(),
				Params: storage.Params{
					"pool": val.DriverOptions["pool"],
				},
			},
			Timeout: dc.Global.Timeout,
		}

		if err := driver.CreateSnapshot(time.Now().String(), driverOpts); err != nil {
			logrus.Errorf("Error creating snapshot for volume %q: %v", val, err)
			return err
		}

		return nil
	})

	if err != nil {
		logrus.Errorf("Error creating snapshot for volume %q: %v", val, err)
	}
}

func (dc *DaemonConfig) loop() {
	for {
		time.Sleep(time.Second)

		// XXX this copy is so we can free the mutex quickly for more additions
		volumeCopy := map[string]*config.Volume{}

		volumeMutex.Lock()
		for volume, val := range volumes {
			volumeCopy[volume] = val
		}
		volumeMutex.Unlock()

		for volume, val := range volumeCopy {
			if isUsed, err := dc.Config.IsVolumeInUse(val); err != nil {
				logrus.Errorf("%s", err) // some issue with "etcd GET"; we should not hit this case
			} else {
				if !isUsed { // volume not in use
					continue
				}
			}
			if val.RuntimeOptions.UseSnapshots {
				freq, err := time.ParseDuration(val.RuntimeOptions.Snapshot.Frequency)
				if err != nil {
					logrus.Errorf("Volume %q has an invalid frequency. Skipping snapshot.", volume)
				}

				if time.Now().Unix()%int64(freq.Seconds()) == 0 {
					go func(val *config.Volume) {
						dc.createSnapshot(val)
						dc.pruneSnapshots(val)
					}(val)
				}
			}
		}
	}
}
