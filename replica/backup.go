package replica

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/openebs/jiva/types"
	"github.com/yasker/backupstore"
)

const (
	snapBlockSize = 2 << 20 // 2MiB
)

/*
type DeltaBlockBackupOperations interface {
	HasSnapshot(id, volumeID string) bool
	CompareSnapshot(id, compareID, volumeID string) (*metadata.Mappings, error)
	OpenSnapshot(id, volumeID string) error
	ReadSnapshot(id, volumeID string, start int64, data []byte) error
	CloseSnapshot(id, volumeID string) error
}
*/

type Backup struct {
	backingFile *BackingFile
	replica     *Replica
	volumeID    string
	snapshotID  string
}

func NewBackup(backingFile *BackingFile) *Backup {
	return &Backup{
		backingFile: backingFile,
	}
}

func (rb *Backup) HasSnapshot(id, volumeID string) bool {
	//TODO Check current in the volume directory of volumeID
	if err := rb.assertOpen(id, volumeID); err != nil {
		return false
	}

	to := rb.findIndex(id)
	if to < 0 {
		return false
	}
	return true
}

func (rb *Backup) OpenSnapshot(id, volumeID string) error {
	if rb.volumeID == volumeID && rb.snapshotID == id {
		return nil
	}

	if rb.volumeID != "" {
		return fmt.Errorf("Volume %s and snapshot %s are already open, close first", rb.volumeID, rb.snapshotID)
	}

	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("Cannot get working directory: %v", err)
	}
	r, err := NewReadOnly(dir, id, rb.backingFile)
	if err != nil {
		return err
	}

	rb.replica = r
	rb.volumeID = volumeID
	rb.snapshotID = id

	return nil
}

func (rb *Backup) assertOpen(id, volumeID string) error {
	if rb.volumeID != volumeID || rb.snapshotID != id {
		return fmt.Errorf("Invalid state volume [%s] and snapshot [%s] are open, not [%s], [%s]", rb.volumeID, rb.snapshotID, id, volumeID)
	}
	return nil
}

func (rb *Backup) ReadSnapshot(id, volumeID string, start int64, data []byte) error {
	if err := rb.assertOpen(id, volumeID); err != nil {
		return err
	}

	if rb.snapshotID != id && rb.volumeID != volumeID {
		return fmt.Errorf("Snapshot %s and volume %s are not open", id, volumeID)
	}

	_, err := rb.replica.ReadAt(data, start)
	return err
}

func (rb *Backup) CloseSnapshot(id, volumeID string) error {
	if rb.volumeID == "" {
		return nil
	}

	if err := rb.assertOpen(id, volumeID); err != nil {
		return err
	}

	err := rb.replica.Close()

	rb.replica = nil
	rb.volumeID = ""
	rb.snapshotID = ""
	return err
}

func (rb *Backup) CompareSnapshot(id, compareID, volumeID string) (*backupstore.Mappings, error) {
	if err := rb.assertOpen(id, volumeID); err != nil {
		return nil, err
	}

	rb.replica.Lock()
	defer rb.replica.Unlock()

	from := rb.findIndex(id)
	if from < 0 {
		return nil, fmt.Errorf("Failed to find snapshot %s in chain", id)
	}

	to := rb.findIndex(compareID)
	if to < 0 {
		return nil, fmt.Errorf("Failed to find snapshot %s in chain", compareID)
	}

	mappings := &backupstore.Mappings{
		BlockSize: snapBlockSize,
	}
	mapping := backupstore.Mapping{
		Offset: -1,
	}

	if err := preload(&rb.replica.volume); err != nil {
		return nil, err
	}

	for i, val := range rb.replica.volume.location {
		if val <= byte(from) && val > byte(to) {
			offset := int64(i) * rb.replica.volume.sectorSize
			// align
			offset -= (offset % snapBlockSize)
			if mapping.Offset != offset {
				mapping = backupstore.Mapping{
					Offset: offset,
					Size:   snapBlockSize,
				}
				mappings.Mappings = append(mappings.Mappings, mapping)
			}
		}
	}

	return mappings, nil
}

func (rb *Backup) findIndex(id string) int {
	if id == "" {
		if rb.backingFile == nil {
			return 0
		}
		return 1
	}

	for i, disk := range rb.replica.activeDiskData {
		if i == 0 {
			continue
		}
		if disk.Name == id {
			return i
		}
	}
	return -1
}

type Hole struct {
	f      types.DiffDisk
	offset int64
	len    int64
}

var HoleCreatorChan = make(chan Hole, 1000000)

func CreateHoles() error {
	retryCount := 0
	for {
		hole := <-HoleCreatorChan
	retry:
		if err := syscall.Fallocate(int(hole.f.Fd()), 0x01|0x02, hole.offset*4096, hole.len*4096); err != nil {
			logrus.Errorf("ERROR in creating hole: %v, Retry_Count: %v", err, retryCount)
			time.Sleep(1)
			retryCount++
			if retryCount == 5 {
				logrus.Fatalf("Error Creating holes: %v", err)
			}
			goto retry
		}
		retryCount = 0
	}
}

func sendToCreateHole(f types.DiffDisk, offset int64, len int64) {
	hole := Hole{
		f:      f,
		offset: offset,
		len:    len,
	}
	HoleCreatorChan <- hole
}

func preload(d *diffDisk) error {
	var file types.DiffDisk
	var length int64
	var lOffset int64
	var fileIndx byte
	var userCreatedSnapIndx byte
	for i, f := range d.files {
		if i == 0 {
			continue
		}
		if i == 1 {
			//TODO Check if this zeroing is needed
			// Reinitialize to zero so that we can detect holes in the base snapshot
			for j := 0; j < len(d.location); j++ {
				d.location[j] = 0
			}
		}
		if d.UserCreatedSnap[i] {
			userCreatedSnapIndx = byte(i)
		}
		generator := newGenerator(d, f)
		for offset := range generator.Generate() {
			if d.location[offset] != 0 {
				if d.files[d.location[offset]] != file {
					if file != nil && fileIndx > userCreatedSnapIndx && !d.ReadOnlyIndx[fileIndx] {
						sendToCreateHole(file, lOffset, length)
					}
					file = d.files[d.location[offset]]
					fileIndx = d.location[offset]
					length = 1
					lOffset = offset
				} else {
					length++
				}
			}
			d.location[offset] = byte(i)
			d.UsedBlocks++
		}
		if file != nil && fileIndx > userCreatedSnapIndx && !d.ReadOnlyIndx[fileIndx] {
			sendToCreateHole(file, lOffset, length)
			file = nil
		}

		if generator.Err() != nil {
			fmt.Println("GENERATOR ERROR")
			return generator.Err()
		}
	}

	return nil
}

func PreloadLunMap(d *diffDisk) error {
	err := preload(d)
	for _, val := range d.location {
		if val != 0 {
			d.UsedLogicalBlocks++
		}
	}
	return err
}
