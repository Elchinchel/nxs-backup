package desc_files

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/mb0/glob"

	"github.com/nixys/nxs-backup/interfaces"
	"github.com/nixys/nxs-backup/misc"
	"github.com/nixys/nxs-backup/modules/backend/exec_cmd"
	"github.com/nixys/nxs-backup/modules/backend/targz"
	"github.com/nixys/nxs-backup/modules/logger"
	"github.com/nixys/nxs-backup/modules/metrics"
)

type job struct {
	name             string
	tmpDir           string
	needToMakeBackup bool
	safetyBackup     bool
	deferredCopying  bool
	diskRateLimit    int64
	storages         interfaces.Storages
	targets          map[string]target
	dumpedObjects    map[string]interfaces.DumpObject
	appMetrics       *metrics.Data
}

type target struct {
	path        string
	gzip        bool
	saveAbsPath bool
	excludes    []string
}

type JobParams struct {
	Name             string
	TmpDir           string
	NeedToMakeBackup bool
	SafetyBackup     bool
	DeferredCopying  bool
	DiskRateLimit    int64
	Storages         interfaces.Storages
	Sources          []SourceParams
	Metrics          *metrics.Data
}

type SourceParams struct {
	Name        string
	Targets     []string
	Excludes    []string
	Gzip        bool
	SaveAbsPath bool
}

func Init(jp JobParams) (interfaces.Job, error) {

	// check if tar and gzip available
	if _, err := exec_cmd.Exec("tar", "--version"); err != nil {
		return nil, fmt.Errorf("Job `%s` init failed. Can't check `tar` version. Please install `tar`. Error: %s ", jp.Name, err)
	}

	j := job{
		name:             jp.Name,
		tmpDir:           jp.TmpDir,
		needToMakeBackup: jp.NeedToMakeBackup,
		safetyBackup:     jp.SafetyBackup,
		deferredCopying:  jp.DeferredCopying,
		diskRateLimit:    jp.DiskRateLimit,
		storages:         jp.Storages,
		targets:          make(map[string]target),
		dumpedObjects:    make(map[string]interfaces.DumpObject),
		appMetrics: jp.Metrics.RegisterJob(
			metrics.JobData{
				JobName:       jp.Name,
				JobType:       misc.DescFiles,
				TargetMetrics: make(map[string]metrics.TargetData),
			},
		),
	}

	for _, src := range jp.Sources {

		for _, targetPattern := range src.Targets {

			for strings.HasSuffix(targetPattern, "/") {
				targetPattern = strings.TrimSuffix(targetPattern, "/")
			}

			targetOfsList, err := filepath.Glob(targetPattern)
			if err != nil {
				return nil, fmt.Errorf("Job `%s` init failed. Unable to process pattern: %s. Error: %s. ", jp.Name, targetPattern, err)
			}

			for _, ofsFullPath := range targetOfsList {
				var excludes []string

				skipOfs := false
				for _, pattern := range src.Excludes {
					match, err := glob.Match(pattern, ofsFullPath)
					if err != nil {
						return nil, fmt.Errorf("Job `%s` init failed. Unable to process pattern: %s. Error: %s. ", jp.Name, pattern, err)
					}
					if match {
						skipOfs = true
					}

					excludes = append(excludes, pattern)
				}

				if !skipOfs {
					ofsPart := misc.GetOfsPart(targetPattern, ofsFullPath)
					ofs := src.Name + "/" + ofsPart

					j.targets[ofs] = target{
						path:        ofsFullPath,
						gzip:        src.Gzip,
						saveAbsPath: src.SaveAbsPath,
						excludes:    excludes,
					}
					j.appMetrics.Job[jp.Name].TargetMetrics[ofs] = metrics.TargetData{
						Source: src.Name,
						Target: ofsPart,
						Values: make(map[string]float64),
					}
				}
			}
		}
	}

	return &j, nil
}

func (j *job) SetOfsMetrics(ofs string, metricsMap map[string]float64) {
	for m, v := range metricsMap {
		j.appMetrics.Job[j.name].TargetMetrics[ofs].Values[m] = v
	}
}

func (j *job) GetName() string {
	return j.name
}

func (j *job) GetTempDir() string {
	return j.tmpDir
}

func (j *job) GetType() misc.BackupType {
	return misc.DescFiles
}

func (j *job) GetTargetOfsList() (ofsList []string) {
	for ofs := range j.targets {
		ofsList = append(ofsList, ofs)
	}
	return
}

func (j *job) GetStoragesCount() int {
	return len(j.storages)
}

func (j *job) GetDumpObjects() map[string]interfaces.DumpObject {
	return j.dumpedObjects
}

func (j *job) SetDumpObjectDelivered(ofs string) {
	dumpObj := j.dumpedObjects[ofs]
	dumpObj.Delivered = true
	j.dumpedObjects[ofs] = dumpObj
}

func (j *job) IsBackupSafety() bool {
	return j.safetyBackup
}

func (j *job) DeleteOldBackups(logCh chan logger.LogRecord, ofsPath string) error {
	logCh <- logger.Log(j.name, "").Debugf("Starting rotate outdated backups.")
	return j.storages.DeleteOldBackups(logCh, j, ofsPath)
}

func (j *job) CleanupTmpData() error {
	return j.storages.CleanupTmpData(j)
}

func (j *job) NeedToMakeBackup() bool {
	return j.needToMakeBackup
}

func (j *job) NeedToUpdateIncMeta() bool {
	return false
}

func (j *job) DoBackup(logCh chan logger.LogRecord, tmpDir string) error {
	var errs *multierror.Error

	for ofsPart, tgt := range j.targets {
		startTime := time.Now()

		j.SetOfsMetrics(ofsPart, map[string]float64{
			metrics.BackupOk:        float64(0),
			metrics.BackupTime:      float64(0),
			metrics.DeliveryOk:      float64(0),
			metrics.DeliveryTime:    float64(0),
			metrics.BackupSize:      float64(0),
			metrics.BackupTimestamp: float64(startTime.Unix()),
		})

		tmpBackupFile := misc.GetFileFullPath(tmpDir, ofsPart, "tar", "", tgt.gzip)
		err := os.MkdirAll(path.Dir(tmpBackupFile), os.ModePerm)
		if err != nil {
			logCh <- logger.Log(j.name, "").Errorf("Unable to create tmp dir with next error: %s", err)
			errs = multierror.Append(errs, err)
			continue
		}

		if err = targz.Tar(targz.TarOpts{
			Src:         tgt.path,
			Dst:         tmpBackupFile,
			Incremental: false,
			Gzip:        tgt.gzip,
			SaveAbsPath: tgt.saveAbsPath,
			RateLim:     j.diskRateLimit,
			Excludes:    tgt.excludes,
		}); err != nil {
			j.SetOfsMetrics(ofsPart, map[string]float64{
				metrics.BackupTime: float64(time.Since(startTime).Nanoseconds() / 1e6),
			})
			logCh <- logger.Log(j.name, "").Errorf("Failed to create temp backup \"%s\". Error: %v", tmpBackupFile, err)
			var serr targz.Error
			if errors.As(err, &serr) {
				logCh <- logger.Log(j.name, "").Debugf("STDERR: %s", serr.Stderr)
			}
			errs = multierror.Append(errs, err)
			continue
		}
		fileInfo, _ := os.Stat(tmpBackupFile)
		j.SetOfsMetrics(ofsPart, map[string]float64{
			metrics.BackupOk:   float64(1),
			metrics.BackupTime: float64(time.Since(startTime).Nanoseconds() / 1e6),
			metrics.BackupSize: float64(fileInfo.Size()),
		})

		logCh <- logger.Log(j.name, "").Debugf("Created temp backup %s", tmpBackupFile)

		j.dumpedObjects[ofsPart] = interfaces.DumpObject{TmpFile: tmpBackupFile}
		if !j.deferredCopying {
			if err = j.storages.Delivery(logCh, j); err != nil {
				logCh <- logger.Log(j.name, "").Errorf("Failed to delivery backup. Errors: %v", err)
				errs = multierror.Append(errs, err)
			}
		}
	}

	if err := j.storages.Delivery(logCh, j); err != nil {
		logCh <- logger.Log(j.name, "").Errorf("Failed to delivery backup. Errors: %v", err)
		errs = multierror.Append(errs, err)
	}

	return errs.ErrorOrNil()
}

func (j *job) Close() error {
	for _, st := range j.storages {
		_ = st.Close()
	}
	return nil
}
