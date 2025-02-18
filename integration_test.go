// Copyright © 2016 Prateek Malhotra (someone1@gmail.com)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	oglog "log"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/storage"
	"github.com/Azure/azure-storage-blob-go/azblob"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/op/go-logging"

	"github.com/jdfalk/zfsbackup-go/backends"
	"github.com/jdfalk/zfsbackup-go/backup"
	"github.com/jdfalk/zfsbackup-go/cmd"
	"github.com/jdfalk/zfsbackup-go/config"
	"github.com/jdfalk/zfsbackup-go/files"
	"github.com/jdfalk/zfsbackup-go/log"
)

const (
	s3TestBucketName    = "s3integrationbuckettest"
	azureTestBucketName = "azureintegrationbuckettest"
	logLevel            = "debug"
)

func setupAzureBucket(t *testing.T) func() {
	t.Helper()
	if os.Getenv("AZURE_CUSTOM_ENDPOINT") == "" {
		t.Skip("No custom Azure Endpoint provided to test against")
	}
	err := os.Setenv("AZURE_ACCOUNT_NAME", storage.StorageEmulatorAccountName)
	if err != nil {
		t.Fatalf("could not set environmental variable due to error: %v", err)
	}
	err = os.Setenv("AZURE_ACCOUNT_KEY", storage.StorageEmulatorAccountKey)
	if err != nil {
		t.Fatalf("could not set environmental variable due to error: %v", err)
	}

	ctx := context.Background()

	credential, err := azblob.NewSharedKeyCredential(storage.StorageEmulatorAccountName, storage.StorageEmulatorAccountKey)
	if err != nil {
		t.Fatalf("failed to parse SAS key: %v", err)
	}
	destURL, err := url.Parse(os.Getenv("AZURE_CUSTOM_ENDPOINT"))
	if err != nil {
		t.Fatalf("failed to construct Azure API URL: %v", err)
	}
	pipeline := azblob.NewPipeline(credential, azblob.PipelineOptions{})
	svcURL := azblob.NewServiceURL(*destURL, pipeline)
	containerSvc := svcURL.NewContainerURL(azureTestBucketName)
	if _, err = containerSvc.Create(ctx, azblob.Metadata{}, azblob.PublicAccessNone); err != nil {
		t.Fatalf("error while creating bucket: %v", err)
	}

	return func() {
		if _, err := containerSvc.Delete(ctx, azblob.ContainerAccessConditions{}); err != nil {
			t.Errorf("could not delete container - %v", err)
		}
	}
}

func setupS3Bucket(t *testing.T) func() {
	if os.Getenv("AWS_S3_CUSTOM_ENDPOINT") == "" {
		t.Skip("No custom S3 Endpoint provided to test against")
	}

	awsconf := aws.NewConfig().
		WithS3ForcePathStyle(true).
		WithEndpoint(os.Getenv("AWS_S3_CUSTOM_ENDPOINT"))

	sess, err := session.NewSession(awsconf)
	if err != nil {
		t.Fatalf("could not create AWS client due to error: %v", err)
	}

	client := s3.New(sess)
	_, err = client.CreateBucket(&s3.CreateBucketInput{
		Bucket: aws.String(s3TestBucketName),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() != "BucketAlreadyOwnedByYou" {
				t.Fatalf("could not create S3 bucket due to error: %v", err)
			}
		}
	}

	return func() {
		objects, err := client.ListObjects(&s3.ListObjectsInput{
			Bucket: aws.String(s3TestBucketName),
		})
		if err != nil {
			t.Errorf("could not list objects: %v", err)
		}

		objectsToDelete := make([]*s3.ObjectIdentifier, 0, len(objects.Contents))
		for _, object := range objects.Contents {
			obj := s3.ObjectIdentifier{
				Key: object.Key,
			}
			objectsToDelete = append(objectsToDelete, &obj)
		}

		if _, err := client.DeleteObjects(&s3.DeleteObjectsInput{
			Bucket: aws.String(s3TestBucketName),
			Delete: &s3.Delete{
				Objects: objectsToDelete,
			},
		}); err != nil {
			t.Errorf("could not delete objects: %v", err)
		}
		if _, err := client.DeleteBucket(&s3.DeleteBucketInput{
			Bucket: aws.String(s3TestBucketName),
		}); err != nil {
			t.Errorf("could not delete bucket - %v", err)
		}
	}
}

func TestVersion(t *testing.T) {
	old := config.Stdout
	buf := bytes.NewBuffer(nil)
	config.Stdout = buf
	defer func() { config.Stdout = old }()

	os.Args = []string{config.ProgramName, "version"}
	main()

	if !strings.Contains(buf.String(), fmt.Sprintf("Version:\tv%s", config.Version())) {
		t.Fatalf("expected version in version command output, did not receive one:\n%s", buf.String())
	}

	buf.Reset()
	os.Args = []string{config.ProgramName, "version", "--jsonOutput"}
	main()
	jout := struct {
		Version string
	}{}
	if err := json.Unmarshal(buf.Bytes(), &jout); err != nil {
		t.Fatalf("expected output to be JSON, got error while trying to decode - %v", err)
	} else if jout.Version != config.Version() {
		t.Fatalf("expected version to be '%s', got '%s' instead", config.Version(), jout.Version)
	}
}

// copyDataset will copy the dataset - useful if tests mess around with the snapshots/bookmarks/options/etc.
func copyDataset(t *testing.T, source, dest string) {
	t.Helper()

	t.Logf("Copying %s to %s", source, dest)

	// nolint:gosec // The input is safe
	sendCMD := exec.Command("zfs", "send", "-R", source)
	receiveCMD := exec.Command("zfs", "receive", dest)
	sendBuf := bytes.NewBuffer(nil)
	recBuf := bytes.NewBuffer(nil)
	sendCMD.Stderr = sendBuf
	receiveCMD.Stderr = recBuf

	var err error
	sendCMD.Stdout, err = receiveCMD.StdinPipe()
	if err != nil {
		t.Fatalf("could not get os pipe: %v", err)
	}

	errChan := make(chan error)
	go func() {
		errChan <- receiveCMD.Run()
	}()

	if sErr := sendCMD.Run(); sErr != nil {
		t.Fatalf("unexpected error sending dataset %s to %s - %v: %s", source, dest, sErr, sendBuf.String())
	}

	if err = <-errChan; err != nil {
		t.Fatalf("unexpected error receiving dataset %s to %s - %v: %s", source, dest, err, recBuf.String())
	}
}

// deleteDataset will do a recursive force delete of the provided pool/dataset
func deleteDataset(t *testing.T, name string) {
	t.Helper()

	// nolint:gosec // The input is safe
	destroyCmd := exec.Command("zfs", "destroy", "-f", "-r", name)
	destroyBuf := bytes.NewBuffer(nil)
	destroyCmd.Stderr = destroyBuf

	if err := destroyCmd.Run(); err != nil {
		t.Fatalf("unexpected error deleting dataset %s - %v: %s", name, err, destroyBuf.String())
	}
}

func compareDirs(t *testing.T, source, dest string) {
	t.Helper()

	// nolint:gosec // The input is safe
	diffCmd := exec.Command("diff", "-rq", "--exclude", ".zfs", source, dest)
	errBuf := bytes.NewBuffer(nil)
	diffCmd.Stderr = errBuf

	if err := diffCmd.Run(); err != nil {
		t.Logf("diff output: %s", errBuf.String())
		t.Fatalf("unexpected difference comparing %s with %s: %v", source, dest, err)
	}
}

func TestIntegration(t *testing.T) {
	ctx := context.Background()

	removeAzureBucket := setupAzureBucket(t)
	defer removeAzureBucket()

	removeS3Bucket := setupS3Bucket(t)
	defer removeS3Bucket()

	s3bucket := fmt.Sprintf("%s://%s", backends.AWSS3BackendPrefix, s3TestBucketName)
	azurebucket := fmt.Sprintf("%s://%s", backends.AzureBackendPrefix, azureTestBucketName)
	bucket := fmt.Sprintf("%s,%s", s3bucket, azurebucket)
	dataset := fmt.Sprintf("tank/%s", t.Name())

	copyDataset(t, "tank/data@c", dataset)
	defer deleteDataset(t, dataset)

	// Azurite doesn't seem to like '|' so making separator '-'
	// Backup Tests
	t.Run("Backup", func(t *testing.T) {
		cmd.ResetSendJobInfo()

		logBuf := bytes.NewBuffer(nil)
		log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(logBuf, "", oglog.Ldate|oglog.Ltime)))

		// Manual Full Backup
		cmd.RootCmd.SetArgs([]string{"send", "--logLevel", logLevel, "--separator", "+", fmt.Sprintf("%s@a", dataset), bucket})
		if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("error performing backup: %v", err)
		}

		cmd.ResetSendJobInfo()

		// Bookmark setup
		// nolint:gosec // The input is safe
		if err := exec.Command("zfs", "bookmark", fmt.Sprintf("%s@a", dataset), fmt.Sprintf("%s#a", dataset)).Run(); err != nil {
			t.Fatalf("unexpected error creating bookmark %s#a: %v", dataset, err)
		}

		// nolint:gosec // The input is safe
		if err := exec.Command("zfs", "destroy", fmt.Sprintf("%s@a", dataset)).Run(); err != nil {
			t.Fatalf("unexpected error destroying snapshot %s@a: %v", dataset, err)
		}

		// Manual Incremental Backup from bookmark
		cmd.RootCmd.SetArgs([]string{"send", "--logLevel", logLevel, "--separator", "+", "-i", fmt.Sprintf("%s#a", dataset), fmt.Sprintf("%s@b", dataset), bucket})
		if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("error performing backup: %v", err)
		}

		cmd.ResetSendJobInfo()

		// Another Bookmark setup
		// nolint:gosec // The input is safe
		if err := exec.Command("zfs", "bookmark", fmt.Sprintf("%s@b", dataset), fmt.Sprintf("%s#b", dataset)).Run(); err != nil {
			t.Fatalf("unexpected error creating bookmark %s#b: %v", dataset, err)
		}

		// nolint:gosec // The input is safe
		if err := exec.Command("zfs", "destroy", fmt.Sprintf("%s@b", dataset)).Run(); err != nil {
			t.Fatalf("unexpected error destroying snapshot %s@b: %v", dataset, err)
		}

		// "Smart" incremental Backup from bookmark
		cmd.RootCmd.SetArgs([]string{"send", "--logLevel", logLevel, "--separator", "+", "--compressor", "xz", "--compressionLevel", "2", "--increment", dataset, bucket})
		if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("error performing backup: %v", err)
		}

		cmd.ResetSendJobInfo()

		// Smart Incremental Backup - Nothing to do
		cmd.RootCmd.SetArgs([]string{"send", "--logLevel", logLevel, "--separator", "+", "--increment", dataset, bucket})
		if err := cmd.RootCmd.ExecuteContext(ctx); err != backup.ErrNoOp {
			t.Fatalf("expecting error %v, but got %v instead", backup.ErrNoOp, err)
		}

		cmd.ResetSendJobInfo()
	})

	restoreTest := []struct {
		backend string
		bucket  string
		target  string
	}{
		{"AWSS3", s3bucket, "tank/data3"},
		{"Azure", azurebucket, "tank/data2"},
	}
	for _, test := range restoreTest {
		t.Run(fmt.Sprintf("List%s", test.backend), listWrapper(dataset, test.bucket))
		t.Run(fmt.Sprintf("Restore%s", test.backend), restoreWrapper(dataset, test.bucket, test.target))
	}
}

func listWrapper(dataset, bucket string) func(*testing.T) {
	return func(t *testing.T) {
		ctx := context.Background()

		logBuf := bytes.NewBuffer(nil)
		log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(logBuf, "", oglog.Ldate|oglog.Ltime)))

		old := config.Stdout
		buf := bytes.NewBuffer(nil)
		config.Stdout = buf
		defer func() { config.Stdout = old }()

		listTests := []struct {
			volumeName string
			after      time.Time
			before     time.Time
			keys       int
			entries    int
		}{
			// volumeName tests
			{"", time.Time{}, time.Time{}, 1, 3},
			{"t*", time.Time{}, time.Time{}, 1, 3},
			{"v*", time.Time{}, time.Time{}, 0, 0},
			{dataset, time.Time{}, time.Time{}, 1, 3},
			{"tan", time.Time{}, time.Time{}, 0, 0},
			// before Tests
			{"", time.Time{}, time.Now(), 1, 3},
			{"", time.Time{}, time.Now().Add(-24 * time.Hour), 0, 0},
			// after Tests
			{"", time.Now().Add(-24 * time.Hour), time.Time{}, 1, 3},
			{"", time.Now(), time.Time{}, 0, 0},
		}

		for _, test := range listTests {
			test := test
			t.Run(test.volumeName, func(t *testing.T) {
				opts := []string{"list", "--logLevel", logLevel, "--jsonOutput"}
				if test.volumeName != "" {
					opts = append(opts, "--volumeName", test.volumeName)
				}
				if !test.after.IsZero() {
					opts = append(opts, "--after", test.after.Format(time.RFC3339[:19]))
				}
				if !test.before.IsZero() {
					opts = append(opts, "--before", test.before.Format(time.RFC3339[:19]))
				}

				cmd.ResetListJobInfo()

				cmd.RootCmd.SetArgs(append(opts, bucket))
				if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
					t.Fatalf("error performing backup: %v", err)
				}

				jout := make(map[string][]*files.JobInfo)
				if err := json.Unmarshal(buf.Bytes(), &jout); err != nil {
					t.Fatalf("error parsing json output: %v", err)
				}

				if len(jout) != test.keys || len(jout[dataset]) != test.entries {
					t.Fatalf("expected %d keys and %d entries, got %d keys and %d entries", test.keys, test.entries, len(jout), len(jout[dataset]))
				}

				if len(jout[dataset]) == 3 {
					if jout[dataset][0].BaseSnapshot.Name != "a" || jout[dataset][1].BaseSnapshot.Name != "b" || jout[dataset][2].BaseSnapshot.Name != "c" {
						t.Fatalf("expected snapshot order a -> b -> c, but got %s -> %s -> %s instead", jout[dataset][0].BaseSnapshot.Name, jout[dataset][1].BaseSnapshot.Name, jout[dataset][2].BaseSnapshot.Name)
					}
				}

				buf.Reset()
			})
		}
	}
}

func restoreWrapper(dataset, bucket, target string) func(*testing.T) {
	return func(t *testing.T) {
		ctx := context.Background()

		logBuf := bytes.NewBuffer(nil)
		log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(logBuf, "", oglog.Ldate|oglog.Ltime)))

		scratchDir, sErr := os.MkdirTemp("", "")
		if sErr != nil {
			t.Fatalf("could not create temp scratch dir: %v", sErr)
		}
		defer os.RemoveAll(scratchDir)
		defer deleteDataset(t, target)

		cmd.ResetReceiveJobInfo()

		// Restore to snapshot @a (full)
		cmd.RootCmd.SetArgs([]string{"receive", "--logLevel", logLevel, "--separator", "+", "-F", fmt.Sprintf("%s@a", dataset), bucket, target})
		if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("error performing receive: %v", err)
		}

		cmd.ResetReceiveJobInfo()

		// Restore to snapshot @b from @a (incremental)
		cmd.RootCmd.SetArgs([]string{"receive", "--logLevel", logLevel, "--separator", "+", "-F", "-i", fmt.Sprintf("%s@a", dataset), fmt.Sprintf("%s@b", dataset), bucket, target})
		if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("error performing receive: %v", err)
		}

		cmd.ResetReceiveJobInfo()

		// Restore to latest snapshot @c (auto)
		cmd.RootCmd.SetArgs([]string{"receive", "--logLevel", logLevel, "--separator", "+", "--workingDirectory", scratchDir, "-F", "--auto", dataset, bucket, target})
		if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("error performing receive: %v", err)
		}

		compareDirs(t, "/tank/data", "/"+target)

		cmd.ResetReceiveJobInfo()

		// Restore to snapshot @c from origin tank/data@b (auto)
		cmd.RootCmd.SetArgs([]string{"receive", "--logLevel", logLevel, "--separator", "+", "-F", "--auto", "-o", "origin=tank/data@b", dataset, bucket, target + "origin"})
		if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("error performing receive: %v", err)
		}

		defer deleteDataset(t, target+"origin")

		compareDirs(t, "/tank/data", "/"+target+"origin")
	}
}

// TestEncryptionAndSign expects private.pgp and public.pgp to be available with the test@example.com user
func TestEncryptionAndSign(t *testing.T) {
	ctx := context.Background()

	tempDir, err := os.MkdirTemp("", t.Name())
	if err != nil {
		t.Fatalf("error preparing temp dir for tests - %v", err)
	}
	defer os.RemoveAll(tempDir) // clean up

	scratchDir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatalf("could not create temp scratch dir: %v", err)
	}
	defer os.RemoveAll(scratchDir)

	var (
		target     = fmt.Sprintf("file://%s", tempDir)
		user       = "test@example.com"
		dataset    = fmt.Sprintf("tank/%s", t.Name())
		newDataset = fmt.Sprintf("tank/%snew", t.Name())
	)

	copyDataset(t, "tank/data@c", dataset)
	defer deleteDataset(t, dataset)
	defer deleteDataset(t, newDataset)

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			"Encrypted Backup - Fail no public keyring",
			[]string{
				"send", "--logLevel", logLevel, "--workingDirectory", scratchDir,
				"--secretKeyRingPath", "private.pgp", "--encryptTo", user, fmt.Sprintf("%s@a", dataset), target,
			},
			true,
		},
		{
			"Signed Backup - Fail no secret keyring",
			[]string{
				"send", "--logLevel", logLevel, "--workingDirectory", scratchDir,
				"--publicKeyRingPath", "public.pgp", "--signFrom", user, fmt.Sprintf("%s@a", dataset), target,
			},
			true,
		},
		{
			"Manual Full Backup - Encrypted",
			[]string{
				"send", "--logLevel", logLevel, "--workingDirectory", scratchDir,
				"--publicKeyRingPath", "public.pgp", "--encryptTo", user, fmt.Sprintf("%s@a", dataset), target,
			},
			false,
		},
		{
			"Manual Incremental Backup - Signed",
			[]string{
				"send", "--logLevel", logLevel, "--workingDirectory", scratchDir,
				"--secretKeyRingPath", "private.pgp", "--signFrom", user, "-i", fmt.Sprintf("%s@a", dataset), fmt.Sprintf("%s@b", dataset), target,
			},
			false,
		},
		{
			"Smart Encrypted Backup - Fail no secret keyring",
			[]string{
				"send", "--logLevel", logLevel, "--workingDirectory", scratchDir,
				"--publicKeyRingPath", "public.pgp", "--encryptTo", user, "--increment", dataset, target,
			},
			true,
		},
		{
			"Smart Encrypted & Signed Backup - Success",
			[]string{
				"send", "--logLevel", logLevel, "--workingDirectory", scratchDir,
				"--publicKeyRingPath", "public.pgp", "--secretKeyRingPath", "private.pgp", "--encryptTo", user, "--signFrom", user, "--increment", dataset, target,
			},
			false,
		},
		{
			"Restore Failure - No Key Ring",
			[]string{"receive", "--logLevel", logLevel, "--workingDirectory", scratchDir, "-F", fmt.Sprintf("%s@a", dataset), target, newDataset},
			true,
		},
		{
			"Full Restore success - Encrypted",
			[]string{"receive", "--logLevel", logLevel, "--workingDirectory", scratchDir, "--secretKeyRingPath", "private.pgp", "--encryptTo", user, "-F", fmt.Sprintf("%s@a", dataset), target, newDataset},
			false,
		},
		{
			"Incremental Restore success - Signed",
			[]string{"receive", "--logLevel", logLevel, "--workingDirectory", scratchDir, "--publicKeyRingPath", "public.pgp", "--signFrom", user, "-F", "-i", fmt.Sprintf("%s@a", dataset), fmt.Sprintf("%s@b", dataset), target, newDataset},
			false,
		},
		{
			"Smart Restore success - Encrypted & Signed",
			[]string{"receive", "--logLevel", logLevel, "--workingDirectory", scratchDir, "--publicKeyRingPath", "public.pgp", "--secretKeyRingPath", "private.pgp", "--encryptTo", user, "--signFrom", user, "-F", "--auto", dataset, target, newDataset},
			false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			switch tt.args[0] {
			case "send":
				cmd.ResetSendJobInfo()
			case "receive":
				cmd.ResetReceiveJobInfo()
			}

			cmd.RootCmd.SetArgs(tt.args)

			buf := bytes.NewBuffer(nil)

			log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(buf, "", oglog.Ldate|oglog.Ltime)))

			if err := cmd.RootCmd.ExecuteContext(ctx); (err != nil) != tt.wantErr {
				t.Logf("%s", buf.String())
				t.Errorf("zfsbackup error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	compareDirs(t, "/tank/data", fmt.Sprintf("/%s", newDataset))
}
