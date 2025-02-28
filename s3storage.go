package github.com/sniranjan/magicstorage

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	cm "github.com/mholt/certmagic"
)

const lockFileExists = "Lock file for already exists"

// staleLockDuration is the length of time
// before considering a lock to be stale.
const staleLockDuration = 2 * time.Hour

// fileLockPollInterval is how frequently
// to check the existence of a lock file
const fileLockPollInterval = 1 * time.Second

var StorageKeys cm.KeyBuilder

// S3Storage implements the certmagic Storage interface using amazon's
// s3 storage.  An effort has been made to make the S3Storage implementation
// as similar as possible to the original filestorage type in order to
// provide a consistent approach to storage backends for certmagic
// for issues, please contact @securityclippy
// S3Storage is safe to use with multiple servers behind an AWS load balancer
// and is safe for concurrent use

type S3Storage struct {
	Path   string
	bucket *string
	svc    s3iface.S3API
}

func NewS3Storage(bucketName, aws_region string) *S3Storage {
	cfg := aws.NewConfig()
	cfg.Region = aws.String(aws_region)
	sess, err := session.NewSession(cfg)
	if err != nil {
		panic(err)
	}
	svc := s3.New(sess)

	store := &S3Storage{
		bucket: aws.String(bucketName),
		svc:    svc,
		Path:   "certmagic",
	}

	return store
}

// Exists returns true if key exists in s3
func (s *S3Storage) Exists(key string) bool {
	_, err := s.svc.GetObject(&s3.GetObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(key),
	})
	if err == nil {
		return true
	}
	aerr, _ := err.(awserr.Error)
	return !(aerr.Code() == s3.ErrCodeNoSuchKey)
}

// Store saves value at key.
func (s *S3Storage) Store(key string, value []byte) error {
	filename := s.Filename(key)
	_, err := s.svc.PutObject(&s3.PutObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(filename),
		Body:   bytes.NewReader(value),
	})

	if err != nil {
		return err
	}
	return nil
}

// Load retrieves the value at key.
func (s *S3Storage) Load(key string) ([]byte, error) {
	result, err := s.svc.GetObject(&s3.GetObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(s.Filename(key)),
	})
	if err != nil {
		return nil, err
	}

	b, err := ioutil.ReadAll(result.Body)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Delete deletes the value at key.
func (s *S3Storage) Delete(key string) error {
	_, err := s.svc.DeleteObject(&s3.DeleteObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(s.Filename(key)),
	})
	if err != nil {
		return err
	}
	return nil
}

// List returns all keys that match prefix.
// because s3 has no concept of directories, everything is an explicit path,
// there is really no such thing as recursive search. This is simply
// here to fulfill the interface requirements of the List function
func (s *S3Storage) List(prefix string, recursive bool) ([]string, error) {
	var keys []string

	prefixPath := s.Filename(prefix)
	result, err := s.svc.ListObjects(&s3.ListObjectsInput{
		Bucket: s.bucket,
		Prefix: aws.String(prefixPath),
	})
	if err != nil {
		return nil, err
	}
	for _, k := range result.Contents {
		if strings.HasPrefix(*k.Key, prefix) {
			keys = append(keys, *k.Key)
		}
	}
	//
	return keys, nil
}

// Stat returns information about key.
func (s *S3Storage) Stat(key string) (cm.KeyInfo, error) {

	result, err := s.svc.GetObject(&s3.GetObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(key),
	})

	if err != nil {
		return cm.KeyInfo{}, err
	}

	return cm.KeyInfo{
		Key:        key,
		Size:       *result.ContentLength,
		Modified:   *result.LastModified,
		IsTerminal: true,
	}, nil
}

// Filename returns the key as a path on the file
// system prefixed by S3Storage.Path.
func (s *S3Storage) Filename(key string) string {
	return filepath.Join(s.Path, filepath.FromSlash(key))
}

// Lock obtains a lock named by the given key. It blocks
// until the lock can be obtained or an error is returned.
func (s *S3Storage) Lock(key string) error {
	start := time.Now()
	lockFile := s.lockFileName(key)

	for {
		err := s.createLockFile(lockFile)
		if err == nil {
			// got the lock, yay
			return nil
		}

		if err.Error() != lockFileExists {
			// unexpected error
			fmt.Println(err)
			return fmt.Errorf("creating lock file: %+v", err)

		}

		// lock file already exists

		info, err := s.Stat(lockFile)
		switch {
		case s.errNoSuchKey(err):
			// must have just been removed; try again to create it
			continue

		case err != nil:
			// unexpected error
			return fmt.Errorf("accessing lock file: %v", err)

		case s.fileLockIsStale(info):
			log.Printf("[INFO][%s] Lock for '%s' is stale; removing then retrying: %s",
				s, key, lockFile)
			s.deleteLockFile(lockFile)
			continue

		case time.Since(start) > staleLockDuration*2:
			// should never happen, hopefully
			return fmt.Errorf("possible deadlock: %s passed trying to obtain lock for %s",
				time.Since(start), key)

		default:
			// lockfile exists and is not stale;
			// just wait a moment and try again
			time.Sleep(fileLockPollInterval)

		}
	}
}

// Unlock releases the lock for name.
func (s *S3Storage) Unlock(key string) error {
	return s.deleteLockFile(s.lockFileName(key))
}

func (s *S3Storage) String() string {
	return "S3Storage:" + s.Path
}

func (s *S3Storage) lockFileName(key string) string {
	return filepath.Join(s.lockDir(), StorageKeys.Safe(key)+".lock")
}

func (s *S3Storage) lockDir() string {
	return filepath.Join(s.Path, "locks")
}

func (s *S3Storage) fileLockIsStale(info cm.KeyInfo) bool {
	return time.Since(info.Modified) > staleLockDuration
}

func (s *S3Storage) createLockFile(filename string) error {
	//lf := s.lockFileName(key)
	exists := s.Exists(filename)
	if exists {
		return fmt.Errorf(lockFileExists)
	}
	_, err := s.svc.PutObject(&s3.PutObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(filename),
		Body:   bytes.NewReader([]byte("lock")),
	})

	if err != nil {
		return err
	}
	return nil
}

func (s *S3Storage) deleteLockFile(keyPath string) error {
	_, err := s.svc.DeleteObject(&s3.DeleteObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(keyPath),
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *S3Storage) errNoSuchKey(err error) bool {
	if err != nil {
		aerr, ok := err.(awserr.Error)
		if !ok {
			return false
		}
		if aerr.Code() == s3.ErrCodeNoSuchKey {
			return true
		}
	}
	return false
}
