package ftp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/textproto"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/jlaffaye/ftp"

	"github.com/nixys/nxs-backup/interfaces"
	"github.com/nixys/nxs-backup/misc"
	"github.com/nixys/nxs-backup/modules/backend/files"
	"github.com/nixys/nxs-backup/modules/logger"
	. "github.com/nixys/nxs-backup/modules/storage"
)

type FTP struct {
	conn       *ftp.ServerConn
	backupPath string
	name       string
	rateLimit  int64
	opts       Opts
	Retention
}

type Opts struct {
	Host              string
	User              string
	Password          string
	Port              int
	ConnectCount      int
	ConnectionTimeout time.Duration
}

func Init(name string, opts Opts, rl int64) (s *FTP, err error) {

	s = &FTP{
		name:      name,
		opts:      opts,
		rateLimit: rl,
	}

	err = s.updateConn()
	if err != nil {
		err = fmt.Errorf("Failed to init '%s' FTP storage. Error: %v ", name, err)
	}

	return
}

func (f *FTP) updateConn() error {

	if f.conn != nil {
		err := f.conn.NoOp()
		if err == nil {
			return nil
		}
	}

	c, err := ftp.Dial(fmt.Sprintf("%s:%d", f.opts.Host, f.opts.Port),
		ftp.DialWithTimeout(f.opts.ConnectionTimeout*time.Second))
	if err != nil {
		return err
	}

	err = c.Login(f.opts.User, f.opts.Password)
	if err != nil {
		return err
	}

	err = c.NoOp()
	if err == nil {
		f.conn = c
	}

	return err
}

func (f *FTP) IsLocal() int { return 0 }

func (f *FTP) SetBackupPath(path string) {
	f.backupPath = path
}

func (f *FTP) SetRateLimit(rl int64) {
	f.rateLimit = rl
}

func (f *FTP) SetRetention(r Retention) {
	f.Retention = r
}

func (f *FTP) DeliveryBackup(logCh chan logger.LogRecord, jobName, tmpBackupFile, ofs string, bakType string) error {
	var bakRemPaths, mtdRemPaths []string

	if err := f.updateConn(); err != nil {
		return err
	}

	if bakType == string(misc.IncFiles) {
		bakRemPaths, mtdRemPaths = GetIncBackupDstList(tmpBackupFile, ofs, f.backupPath)
	} else {
		bakRemPaths = GetDescBackupDstList(tmpBackupFile, ofs, f.backupPath, f.Retention)
	}

	if len(mtdRemPaths) > 0 {
		for _, dstPath := range mtdRemPaths {
			if err := f.copy(logCh, jobName, dstPath, tmpBackupFile+".inc"); err != nil {
				return err
			}
		}
	}

	for _, dstPath := range bakRemPaths {
		if err := f.copy(logCh, jobName, dstPath, tmpBackupFile); err != nil {
			return err
		}
	}

	return nil
}

func (f *FTP) copy(logCh chan logger.LogRecord, job, dst, src string) error {

	// Make remote directories
	dstDir := path.Dir(dst)
	if err := f.mkDir(dstDir); err != nil {
		logCh <- logger.Log(job, f.name).Errorf("Unable to create remote directory '%s': '%s'", dstDir, err)
		return err
	}

	srcFile, err := files.GetLimitedFileReader(src, f.rateLimit)
	if err != nil {
		logCh <- logger.Log(job, f.name).Errorf("Unable to open file: '%s'", err)
		return err
	}
	defer func() { _ = srcFile.Close() }()

	err = f.conn.Stor(dst, srcFile)
	if err != nil {
		logCh <- logger.Log(job, f.name).Errorf("Unable to upload file: %s", err)
		return err
	}

	logCh <- logger.Log(job, f.name).Infof("Successfully uploaded file '%s'", dst)
	return nil
}

func (f *FTP) DeleteOldBackups(logCh chan logger.LogRecord, ofsPart string, job interfaces.Job, full bool) error {

	if err := f.updateConn(); err != nil {
		return err
	}

	if job.GetType() == misc.IncFiles {
		return f.deleteIncBackup(logCh, job.GetName(), ofsPart, full)
	} else {
		return f.deleteDescBackup(logCh, job.GetName(), ofsPart, job.IsBackupSafety())
	}
}

func (f *FTP) deleteDescBackup(logCh chan logger.LogRecord, job, ofsPart string, safety bool) error {
	var errs *multierror.Error

	for _, p := range RetentionPeriodsList {
		retentionCount, retentionDate := GetRetention(p, f.Retention)
		if retentionCount == 0 && retentionDate.IsZero() {
			continue
		}

		bakDir := path.Join(f.backupPath, ofsPart, p.String())
		files, err := f.conn.List(bakDir)
		if err != nil {
			var protoErr *textproto.Error
			errors.As(err, &protoErr)
			if protoErr.Code == 550 {
				continue
			}
			logCh <- logger.Log(job, f.name).Errorf("Failed to read files in remote directory '%s' with next error: %s", bakDir, err)
			return err
		}

		if f.Retention.UseCount {
			sort.Slice(files, func(i, j int) bool {
				return files[i].Time.Before(files[j].Time)
			})
			if !safety {
				retentionCount--
			}
			if retentionCount <= len(files) {
				files = files[:len(files)-retentionCount]
			} else {
				files = files[:0]
			}
		} else {
			i := 0
			for _, file := range files {
				if file.Time.Before(retentionDate) {
					files[i] = file
					i++
				}
			}
			files = files[:i]
		}

		for _, file := range files {
			if file.Name == ".." || file.Name == "." {
				continue
			}

			err = f.conn.Delete(path.Join(bakDir, file.Name))
			if err != nil {
				logCh <- logger.Log(job, f.name).Errorf("Failed to delete file '%s' in remote directory '%s' with next error: %s",
					file.Name, bakDir, err)
				errs = multierror.Append(errs, err)
			} else {
				logCh <- logger.Log(job, f.name).Infof("Deleted old backup file '%s' in remote directory '%s'", file.Name, bakDir)
			}
		}
	}

	return errs.ErrorOrNil()
}

func (f *FTP) deleteIncBackup(logCh chan logger.LogRecord, job, ofsPart string, full bool) error {
	var errs *multierror.Error

	if full {
		backupDir := path.Join(f.backupPath, ofsPart)

		if err := f.conn.ChangeDir(backupDir); err != nil {
			protoErr := err.(*textproto.Error)
			if protoErr.Code == 550 {
				return nil
			} else {
				logCh <- logger.Log(job, f.name).Errorf("Failed to get access to directory '%s' with next error: %v", backupDir, err)
				return err
			}
		}

		if err := f.conn.RemoveDirRecur(backupDir); err != nil {
			logCh <- logger.Log(job, f.name).Errorf("Failed to delete '%s' with next error: %s", backupDir, err)
			errs = multierror.Append(errs, err)
		}
	} else {
		intMoy, _ := strconv.Atoi(misc.GetDateTimeNow("moy"))
		lastMonth := intMoy - f.Months

		var year string
		if lastMonth > 0 {
			year = misc.GetDateTimeNow("year")
		} else {
			year = misc.GetDateTimeNow("previous_year")
			lastMonth += 12
		}

		backupDir := path.Join(f.backupPath, ofsPart, year)

		dirs, err := f.conn.List(backupDir)
		if err != nil {
			logCh <- logger.Log(job, f.name).Errorf("Failed to get access to directory '%s' with next error: %v", backupDir, err)
			return err
		}
		rx := regexp.MustCompile(`month_\d\d`)
		for _, dir := range dirs {
			if rx.MatchString(dir.Name) {
				dirParts := strings.Split(dir.Name, "_")
				dirMonth, _ := strconv.Atoi(dirParts[1])
				if dirMonth < lastMonth {
					if err = f.conn.RemoveDirRecur(path.Join(backupDir, dir.Name)); err != nil {
						logCh <- logger.Log(job, f.name).Errorf("Failed to delete '%s' in dir '%s' with next error: %s",
							dir.Name, backupDir, err)
						errs = multierror.Append(errs, err)
					} else {
						logCh <- logger.Log(job, f.name).Infof("Deleted old backup '%s' in directory '%s'", dir.Name, backupDir)
					}
				}
			}
		}
	}

	return errs.ErrorOrNil()
}

func (f *FTP) mkDir(dstPath string) error {

	dstPath = path.Clean(dstPath)
	if dstPath == "." || dstPath == "/" {
		return nil
	}

	if dstPath != f.backupPath {
		err := f.mkDir(path.Dir(dstPath))
		if err != nil {
			return err
		}
	}

	// skip if dir entry already exist
	if e, err := f.conn.GetEntry(dstPath); err == nil && e.Type == ftp.EntryTypeFolder {
		return nil
	} else if e != nil {
		return errors.New("Can't crate directory. File with the same name already exist. ")
	}

	return f.conn.MakeDir(dstPath)
}

func (f *FTP) GetFileReader(ofsPath string) (io.Reader, error) {
	if err := f.updateConn(); err != nil {
		return nil, err
	}

	// return fs.ErrNotExist if entry not available
	if _, err := f.conn.GetEntry(path.Join(f.backupPath, ofsPath)); err != nil {
		protoErr := err.(*textproto.Error)
		if protoErr.Code == 550 {
			return nil, fs.ErrNotExist
		} else {
			return nil, err
		}
	}

	r, err := f.conn.Retr(path.Join(f.backupPath, ofsPath))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()

	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	// some ftp servers returns empty reader without error if file doesn't exist
	if len(buf) == 0 {
		return nil, errors.New("File empty or doesn't exist ")
	}

	return bytes.NewReader(buf), nil
}

func (f *FTP) Close() error {
	return f.conn.Quit()
}

func (f *FTP) Clone() interfaces.Storage {
	cl := *f
	return &cl
}

func (f *FTP) GetName() string {
	return f.name
}
