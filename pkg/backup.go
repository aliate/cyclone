package pkg

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/backups"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/imagedata"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/containers"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/objects"
	backups_utils "github.com/gophercloud/utils/openstack/blockstorage/extensions/backups"
	images_utils "github.com/gophercloud/utils/openstack/imageservice/v2/images"
	"github.com/klauspost/compress/zlib"
	"github.com/machinebox/progress"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	waitForBackupSec float64
)

const (
	backupChunk      = 52428800
	sha256chunk      = 32768
	compressionLevel = 6 // comply with default python level 6
	backupTimeFormat = "20060102150405"
)

func createBackupSpeed(client *gophercloud.ServiceClient, backup *backups.Backup) {
	if client != nil {
		container, err := containers.Get(client, backup.Container, nil).Extract()
		if err != nil {
			log.Printf("Failed to detect a backup container size: %s", err)
			return
		}
		t := backup.UpdatedAt.Sub(backup.CreatedAt)
		log.Printf("Time to create a backup: %s", t)
		size := float64(container.BytesUsed / (1024 * 1024))
		log.Printf("Size of the backup: %.2f Mb", size)
		log.Printf("Speed of the backup creation: %.2f Mb/sec", size/t.Seconds())
	}
}

func waitForBackup(client *gophercloud.ServiceClient, id string, secs float64) (*backups.Backup, error) {
	var backup *backups.Backup
	var err error
	err = gophercloud.WaitFor(int(secs), func() (bool, error) {
		backup, err = backups.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Intermediate backup status: %s", backup.Status)
		if backup.Status == "available" {
			return true, nil
		}

		if strings.Contains(backup.Status, "error") {
			return false, fmt.Errorf("intermediate backup status is %q", backup.Status)
		}

		// continue status checks
		return false, nil
	})

	return backup, err
}

// calculate sha256 hashes in parallel
func calcSha256Hash(myChunk []byte, hashChan chan [][32]byte) {
	var lenght int = len(myChunk)
	var hashes int
	if n, mod := lenght/sha256chunk, lenght%sha256chunk; mod > 0 {
		hashes = n + 1
	} else {
		hashes = n
	}

	h := make([][32]byte, hashes)
	sha256calc := func(i, start, end int, wg *sync.WaitGroup) {
		h[i] = sha256.Sum256(myChunk[start:end])
		wg.Done()
	}

	var waitGroup sync.WaitGroup
	for i := 0; i < hashes; i++ {
		start := i * sha256chunk
		end := start + sha256chunk
		if end > lenght {
			end = lenght
		}
		waitGroup.Add(1)
		go sha256calc(i, start, end, &waitGroup)
	}

	waitGroup.Wait()

	hashChan <- h
}

func processChunk(wg *sync.WaitGroup, i int, path, containerName string, objClient *gophercloud.ServiceClient, reader *progress.Reader, meta *metadata, sha256meta *sha256file, contChan chan bool, limitChan chan struct{}, errChan chan error) {
	defer func() {
		wg.Done()
		<-limitChan
	}()

	myChunk, err := ioutil.ReadAll(io.LimitReader(reader, backupChunk))
	if err != nil {
		if err != io.EOF {
			errChan <- fmt.Errorf("failed to read file: %s", err)
			return
		}
		// stop further reading, end of file
		contChan <- false
	}
	if len(myChunk) == 0 {
		// stop further reading, no further data
		contChan <- false
		return
	}
	contChan <- true

	chunkPath := fmt.Sprintf("%s-%05d", path, i)
	md5done := make(chan bool)
	// calculate md5 hash while we upload chunks
	go func(myChunk []byte) {
		hash := md5.Sum(myChunk)
		object := backupChunkEntry{
			chunkPath: {
				"compression": "zlib",
				"length":      len(myChunk),
				"md5":         hex.EncodeToString(hash[:]),
				"offset":      (i - 1) * backupChunk,
			},
		}
		meta.Lock()
		meta.Objects[i] = object
		meta.Unlock()
		md5done <- true
	}(myChunk)

	// calculate sha256 hash while we upload chunks
	sha256hashes := make(chan [][32]byte)
	go calcSha256Hash(myChunk, sha256hashes)

	rb := new(bytes.Buffer)
	zf, err := zlib.NewWriterLevel(rb, compressionLevel)
	if err != nil {
		errChan <- fmt.Errorf("failed to set zlib %d compression level: %s", compressionLevel, err)
		return
	}
	_, err = zf.Write(myChunk)
	if err != nil {
		errChan <- fmt.Errorf("failed to write zlib compressed data: %s", err)
		return
	}
	err = zf.Close()
	if err != nil {
		errChan <- fmt.Errorf("failed to flush and close zlib compressed data: %s", err)
		return
	}
	// free up the compressor
	zf.Reset(nil)

	// TODO: check if file exists
	// upload and retry when upload fails
	var retries int = 5
	var sleepSeconds time.Duration = 15
	for j := 0; j < retries; j++ {
		uploadOpts := objects.CreateOpts{
			Content: bytes.NewReader(rb.Bytes()),
		}
		err = objects.Create(objClient, containerName, chunkPath, uploadOpts).Err
		if err != nil {
			log.Printf("failed to upload %q/%q data in %d retry: %s: sleeping %d seconds", containerName, chunkPath, j, err, sleepSeconds)
			time.Sleep(sleepSeconds * time.Second)
			continue
		}
		break
	}
	// free up the buffer
	rb.Reset()

	if err != nil {
		errChan <- fmt.Errorf("failed to upload %q/%q data: %s", containerName, chunkPath, err)
		return
	}

	<-md5done
	sha256meta.Lock()
	sha256meta.Sha256s[i] = <-sha256hashes
	sha256meta.Unlock()

	myChunk = nil
}

func uploadBackup(srcImgClient, srcObjClient, dstObjClient, dstVolClient *gophercloud.ServiceClient, containerName, imageID, az string, properties map[string]string, size int, threads uint) (*backups.Backup, error) {
	imageData, err := getSourceData(srcImgClient, srcObjClient, imageID)
	if err != nil {
		return nil, err
	}
	defer imageData.readCloser.Close()

	if len(properties) > 0 {
		imageData.properties = properties
	}

	if size == 0 {
		if imageData.minDisk == 0 {
			return nil, fmt.Errorf("target volume size cannot be zero")
		}
		size = imageData.minDisk
	}

	if imageData.minDisk > size {
		return nil, fmt.Errorf("cannot create a backup with the size less than the source image min_disk=%d > %d", imageData.minDisk, size)
	}

	progressReader := progress.NewReader(imageData.readCloser)
	go func() {
		for p := range progress.NewTicker(context.Background(), progressReader, imageData.size, 1*time.Second) {
			log.Printf("Progress: %d/%d (%.2f%%), remaining: %s", p.N(), p.Size(), p.Percent(), p.Remaining().Round(time.Second))
		}
	}()

	var volumeID, backupID string
	// generate a new volume UUID
	if v, err := uuid.NewUUID(); err != nil {
		return nil, fmt.Errorf("failed to generate a new volume UUID: %s", err)
	} else {
		volumeID = v.String()
	}

	// generate a new backup UUID
	if v, err := uuid.NewUUID(); err != nil {
		return nil, fmt.Errorf("failed to generate a new backup UUID: %s", err)
	} else {
		backupID = v.String()
	}

	path := fmt.Sprintf("volume_%s/%s/az_%s_backup_%s", volumeID, time.Now().UTC().Format(backupTimeFormat), az, backupID)
	sha256meta := sha256file{
		VolumeID:  volumeID,
		BackupID:  backupID,
		ChunkSize: sha256chunk,
		CreatedAt: time.Now().UTC(),
		Version:   "1.0.0",
		Sha256s:   make(map[int][][32]byte),
	}

	volMeta := volumeMeta{
		Version: 2,
		VolumeBaseMeta: volumeBaseMeta{
			Bootable: len(imageData.properties) > 0,
		},
		VolumeGlanceMetadata: imageData.properties,
	}
	jd, err := json.Marshal(volMeta)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal meta")
	}

	meta := metadata{
		CreatedAt:  sha256meta.CreatedAt,
		Version:    sha256meta.Version,
		VolumeID:   sha256meta.VolumeID,
		VolumeMeta: string(jd),
		Objects:    make(map[int]backupChunkEntry),
	}

	// create container
	_, err = containers.Create(dstObjClient, containerName, nil).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create a %q container: %s", containerName, err)
	}

	var waitGroup sync.WaitGroup
	var i int

	errChan := make(chan error)
	contChan := make(chan bool)
	limitChan := make(chan struct{}, threads)

	// first chunk
	waitGroup.Add(1)
	i++
	go processChunk(&waitGroup, i, path, containerName, dstObjClient, progressReader, &meta, &sha256meta, contChan, limitChan, errChan)
	limitChan <- struct{}{}
L:
	for {
		select {
		case v := <-errChan:
			return nil, v
		case v := <-contChan:
			if !v {
				break L
			}

			limitChan <- struct{}{}
			waitGroup.Add(1)
			i++
			go processChunk(&waitGroup, i, path, containerName, dstObjClient, progressReader, &meta, &sha256meta, contChan, limitChan, errChan)
		}
	}
	log.Printf("Uploading the rest and the metadata")
	waitGroup.Wait()
	imageData.readCloser.Close()

	// write _sha256file
	buf, err := json.MarshalIndent(&sha256meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal sha256meta: %s", err)
	}

	createOpts := objects.CreateOpts{
		Content: bytes.NewReader(buf),
	}
	p := path + "_sha256file"
	err = objects.Create(dstObjClient, containerName, p, createOpts).Err
	if err != nil {
		return nil, fmt.Errorf("failed to upload %q/%q data: %s", containerName, p, err)
	}
	// free up the heap
	buf = nil

	// write _metadata
	buf, err = json.MarshalIndent(&meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal meta: %s", err)
	}

	createOpts = objects.CreateOpts{
		Content: bytes.NewReader(buf),
	}
	p = path + "_metadata"
	err = objects.Create(dstObjClient, containerName, p, createOpts).Err
	if err != nil {
		return nil, fmt.Errorf("failed to upload %q/%q data: %s", containerName, p, err)
	}
	// free up the heap
	buf = nil

	// import the backup
	service := "cinder.backup.drivers.swift.SwiftBackupDriver"
	backupImport := backups.ImportBackup{
		ID:               backupID,
		VolumeID:         volumeID,
		AvailabilityZone: &az,
		UpdatedAt:        time.Now().UTC(),
		ServiceMetadata:  &path,
		Size:             &size,
		ObjectCount:      &i,
		Container:        &containerName,
		Service:          &service,
		CreatedAt:        time.Now().UTC(),
		DataTimestamp:    time.Now().UTC(),
	}

	backupURL, err := json.Marshal(backupImport)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal backupURL: %s", err)
	}

	options := backups.ImportOpts{
		BackupService: service,
		BackupURL:     backupURL,
	}
	importResponse, err := backups.Import(dstVolClient, options).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to import the backup: %s", err)
	}

	backupObj, err := waitForBackup(dstVolClient, importResponse.ID, waitForBackupSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for backup status: %s", err)
	}

	measureTime("Backup upload time: %s")

	return backupObj, nil
}

func backupToVolume(dstVolClient *gophercloud.ServiceClient, backupObj *backups.Backup, volumeName, az string) (*volumes.Volume, error) {
	// create a volume from a backup
	dstVolClient.Microversion = "3.47"
	volOpts := volumes.CreateOpts{
		Name:             volumeName,
		Size:             backupObj.Size,
		Description:      fmt.Sprintf("a volume restored from a %s backup", backupObj.ID),
		AvailabilityZone: az,
		BackupID:         backupObj.ID,
	}

	newVolume, err := volumes.Create(dstVolClient, volOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create a source volume from a backup: %s", err)
	}

	newVolume, err = waitForVolume(dstVolClient, newVolume.ID, waitForVolumeSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a volume: %s", err)
	}

	return newVolume, nil
}

type imageSource struct {
	readCloser io.ReadCloser
	size       int64
	properties map[string]string
	minDisk    int
}

func getSourceData(srcImgClient, srcObjClient *gophercloud.ServiceClient, imageID string) (*imageSource, error) {
	// read file
	file, err := os.Open(imageID)
	if err == nil {
		if fi, err := file.Stat(); err == nil {
			return &imageSource{file, fi.Size(), nil, 0}, nil
		} else {
			log.Printf("Failed to get %q filename size: %s", imageID, err)
		}
		return &imageSource{file, 0, nil, 0}, nil
	}

	log.Printf("Cannot read %q file: %s: fallback to Swift URL as a source", imageID, err)
	// read Glance image metadata
	image, err := images.Get(srcImgClient, imageID).Extract()
	if err != nil {
		return nil, fmt.Errorf("error getting the source image: %s", err)
	}
	properties := expandImageProperties(image.Properties)

	if srcObjClient != nil {
		// read Glance image Swift source
		resp := objects.Download(srcObjClient, fmt.Sprintf("glance_%s", imageID), imageID, nil)
		if resp.Err == nil {
			if size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); size > 0 {
				return &imageSource{
					resp.Body,
					size,
					properties,
					image.MinDiskGigabytes,
				}, nil
			} else if err != nil {
				log.Printf("Failed to detect %q image size: %s: fallback to %d", imageID, err, image.SizeBytes)
			} else {
				log.Printf("Failed to detect %q image size: %d is <= 0: fallback to %d", imageID, size, image.SizeBytes)
			}
			return &imageSource{
				resp.Body,
				image.SizeBytes,
				properties,
				image.MinDiskGigabytes,
			}, nil
		}
		log.Printf("Cannot read Swift URL as a source: %s, fallback to Glance as a source", resp.Err)
	}

	// read Glance image
	readCloser, err := imagedata.Download(srcImgClient, imageID).Extract()
	if err != nil {
		return nil, fmt.Errorf("error getting the source image reader: %s", err)
	}

	return &imageSource{
		readCloser,
		image.SizeBytes,
		properties,
		image.MinDiskGigabytes,
	}, nil
}

// BackupCmd represents the backup command
var BackupCmd = &cobra.Command{
	Use: "backup",
}

var BackupUploadCmd = &cobra.Command{
	Use:   "upload <filename|image_name|image_id>",
	Args:  cobra.ExactArgs(1),
	Short: "Upload an image into a backup",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		image := args[0]

		toName := viper.GetString("to-volume-name")
		toContainerName := viper.GetString("to-container-name")
		size := viper.GetUint("volume-size")
		threads := viper.GetUint("threads")
		toAZ := viper.GetString("to-az")
		restoreVolume := viper.GetBool("restore-volume")
		properties := viper.GetStringMapString("property")

		if toContainerName == "" {
			return fmt.Errorf("Swift container name connot be empty")
		}

		// source and destination parameters
		loc, err := getSrcAndDst("")
		if err != nil {
			return err
		}

		srcProvider, err := NewOpenStackClient(loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %s", err)
		}

		srcObjectClient, err := NewObjectStorageV1Client(srcProvider, loc.Src.Region)
		if err != nil {
			// don't fail, will use Glance client instead
			log.Printf("Failed to create source object storage client: %s", err)
		}

		srcImageClient, err := NewGlanceV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source image client: %s", err)
		}

		// resolve image name to an ID
		if v, err := images_utils.IDFromName(srcImageClient, image); err == nil {
			image = v
		}

		dstProvider, err := NewOpenStackClient(loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %s", err)
		}

		dstVolumeClient, err := NewBlockStorageV3Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination volume client: %s", err)
		}

		dstObjectClient, err := NewObjectStorageV1Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination object storage client, detailed image clone statistics will be unavailable: %s", err)
		}

		err = checkAvailabilityZone(dstVolumeClient, "", &toAZ, &loc)
		if err != nil {
			return err
		}

		defer measureTime()

		backup, err := uploadBackup(srcImageClient, srcObjectClient, dstObjectClient, dstVolumeClient, toContainerName, image, toAZ, properties, int(size), threads)
		if err != nil {
			return err
		}

		if !restoreVolume {
			return nil
		}

		// reauth before the long-time task
		dstVolumeClient.TokenID = ""
		_, err = backupToVolume(dstVolumeClient, backup, toName, toAZ)
		return err
	},
}

var BackupRestoreCmd = &cobra.Command{
	Use:   "restore <backup_name|backup_id>",
	Args:  cobra.ExactArgs(1),
	Short: "Restore a backup into a volume",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		backup := args[0]

		toName := viper.GetString("to-volume-name")
		size := viper.GetUint("volume-size")
		toAZ := viper.GetString("to-az")

		// source and destination parameters
		loc, err := getSrcAndDst("")
		if err != nil {
			return err
		}

		dstProvider, err := NewOpenStackClient(loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %s", err)
		}

		dstVolumeClient, err := NewBlockStorageV3Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination volume client: %s", err)
		}

		err = checkAvailabilityZone(dstVolumeClient, "", &toAZ, &loc)
		if err != nil {
			return err
		}

		// resolve backup name to an ID
		if v, err := backups_utils.IDFromName(dstVolumeClient, backup); err == nil {
			backup = v
		}

		backupObj, err := waitForBackup(dstVolumeClient, backup, waitForBackupSec)
		if err != nil {
			return fmt.Errorf("failed to wait for backup status: %s", err)
		}

		if backupObj.Size == 0 {
			return fmt.Errorf("target volume size must be specified")
		}

		if size > 0 {
			if int(size) < backupObj.Size {
				return fmt.Errorf("target volume size must not be less than %d", backupObj.Size)
			}
			backupObj.Size = int(size)
		}

		defer measureTime()

		_, err = backupToVolume(dstVolumeClient, backupObj, toName, toAZ)
		return err
	},
}

func init() {
	initBackupCmdFlags()
	BackupCmd.AddCommand(BackupUploadCmd)
	BackupCmd.AddCommand(BackupRestoreCmd)
	RootCmd.AddCommand(BackupCmd)
}

func initBackupCmdFlags() {
	BackupUploadCmd.Flags().StringP("to-container-name", "", "", "destination backup Swift container name")
	BackupUploadCmd.Flags().StringP("to-az", "", "", "destination availability zone")
	BackupUploadCmd.Flags().UintP("threads", "t", 1, "an amount of parallel threads")
	BackupUploadCmd.Flags().BoolP("restore-volume", "", false, "restore a volume after upload")
	BackupUploadCmd.Flags().StringP("to-volume-name", "", "", "target volume name")
	BackupUploadCmd.Flags().UintP("volume-size", "b", 0, "target volume size (must not be less than original image virtual size)")
	BackupUploadCmd.Flags().StringToStringP("property", "p", nil, "image property for the target volume")

	BackupRestoreCmd.Flags().StringP("to-volume-name", "", "", "destination backup name")
	BackupRestoreCmd.Flags().StringP("to-az", "", "", "destination availability zone")
	BackupRestoreCmd.Flags().UintP("volume-size", "b", 0, "target volume size")
}
