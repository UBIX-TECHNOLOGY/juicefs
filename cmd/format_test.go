/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
)

type unsupportedTierStorage struct {
	object.ObjectStorage
	initCalls *int
}

func (s *unsupportedTierStorage) InitTiers(_ object.Tiers) error {
	*s.initCalls = *s.initCalls + 1
	return errors.New("not supported")
}

func (s *unsupportedTierStorage) GetTier(context.Context) object.Tier {
	return object.Tier{}
}

func TestFixObjectSize(t *testing.T) {
	t.Run("Should make sure the size is in range", func(t *testing.T) {
		cases := []struct {
			input, expected uint64
		}{
			{30 << 10, 64 << 10},
			{0, 64 << 10},
			{2 << 40, 16 << 20},
			{16 << 21, 16 << 20},
		}
		for _, c := range cases {
			if size := fixObjectSize(c.input); size != c.expected {
				t.Fatalf("Expected %d, got %d", c.expected, size)
			}
		}
	})
	t.Run("Should use powers of two", func(t *testing.T) {
		cases := []struct {
			input, expected uint64
		}{
			{150 << 10, 128 << 10},
			{99 << 10, 64 << 10},
			{1077 << 10, 1024 << 10},
		}
		for _, c := range cases {
			if size := fixObjectSize(c.input); size != c.expected {
				t.Fatalf("Expected %d, got %d", c.expected, size)
			}
		}
	})
}

func TestCreateStorageTierWarning(t *testing.T) {
	const storageName = "unsupported-tier-test"
	initCalls := 0
	object.Register(storageName, func(bucket, accessKey, secretKey, token string) (object.ObjectStorage, error) {
		storage, err := object.CreateStorage("mem", bucket, accessKey, secretKey, token)
		if err != nil {
			return nil, err
		}
		return &unsupportedTierStorage{ObjectStorage: storage, initCalls: &initCalls}, nil
	})

	var logs bytes.Buffer
	originalOutput := logger.Out
	logger.SetOutput(&logs)
	defer logger.SetOutput(originalOutput)

	format := meta.Format{Name: "test", Storage: storageName, Tiers: object.NewTiers("")}
	if _, err := createStorage(format); err != nil {
		t.Fatalf("create storage without configured tiers: %s", err)
	}
	if initCalls != 1 {
		t.Fatalf("InitTiers called %d times, expected 1", initCalls)
	}
	if strings.Contains(logs.String(), "Set storage tier:") {
		t.Fatalf("unexpected storage tier warning: %s", logs.String())
	}

	logs.Reset()
	format.Tiers[0] = object.Tier{ID: 0, Sc: "STANDARD_IA"}
	if _, err := createStorage(format); err != nil {
		t.Fatalf("create storage with a configured tier: %s", err)
	}
	if initCalls != 2 {
		t.Fatalf("InitTiers called %d times, expected 2", initCalls)
	}
	if !strings.Contains(logs.String(), "Set storage tier: not supported") {
		t.Fatalf("missing storage tier warning: %s", logs.String())
	}
}

func TestFormat(t *testing.T) {
	rdb := resetTestMeta()
	if err := Main([]string{"", "format", "--bucket", t.TempDir(), testMeta, testVolume}); err != nil {
		t.Fatalf("format error: %s", err)
	}
	body, err := rdb.Get(context.Background(), "setting").Bytes()
	if err != nil {
		t.Fatalf("get setting: %s", err)
	}
	f := meta.Format{}
	if err = json.Unmarshal(body, &f); err != nil {
		t.Fatalf("json unmarshal: %s", err)
	}
	if f.Name != testVolume {
		t.Fatalf("volume name %s != expected %s", f.Name, testVolume)
	}

	if err = Main([]string{"", "format", testMeta, testVolume, "--capacity", "1", "--inodes", "1000"}); err != nil {
		t.Fatalf("format error: %s", err)
	}
	if body, err = rdb.Get(context.Background(), "setting").Bytes(); err != nil {
		t.Fatalf("get setting: %s", err)
	}
	if err = json.Unmarshal(body, &f); err != nil {
		t.Fatalf("json unmarshal: %s", err)
	}
	if f.Capacity != 1<<30 || f.Inodes != 1000 {
		t.Fatalf("unexpected volume: %+v", f)
	}
}

func TestFormatVersion(t *testing.T) {
	metaURL := "sqlite3://" + filepath.Join(t.TempDir(), "test.db")
	bucket := filepath.Join(t.TempDir(), "testBucket")
	if err := Main([]string{"", "format", metaURL, "--bucket", bucket, testVolume}); err != nil {
		t.Fatalf("format: %s", err)
	}
	if err := Main([]string{"", "config", metaURL, "--min-client-version", "99.0.0", "--yes"}); err != nil {
		t.Fatalf("config: %s", err)
	}

	err := Main([]string{"", "format", metaURL, "--bucket", t.TempDir(), testVolume})
	if err == nil || !strings.Contains(err.Error(), "allowed minimum version: 99.0.0") {
		t.Fatalf("format error %q does not report the version policy", err)
	}

	data, err := getStdout([]string{"", "config", metaURL})
	if err != nil {
		t.Fatalf("get config: %s", err)
	}
	var format meta.Format
	if err = json.Unmarshal(data, &format); err != nil {
		t.Fatalf("json unmarshal: %s", err)
	}
	if format.Bucket != bucket+"/" {
		t.Fatalf("bucket %q != expect %q", format.Bucket, bucket+"/")
	}

	format.MetaVersion = meta.MaxVersion + 1
	if err = checkFormatVersion(&format, true); err == nil {
		t.Fatal("force bypassed incompatible metadata version")
	}
}

func TestClusterForEncryptedFormat(t *testing.T) {
	f := meta.Format{
		Name:        "test",
		UUID:        "UUID-A",
		Storage:     "file",
		Bucket:      "/tmp/top/",
		SecretKey:   "top-secret",
		EncryptAlgo: object.AES256GCM_RSA,
	}
	f.ObjectClusters = []meta.ObjectCluster{{
		ID:           1,
		Storage:      "file",
		Bucket:       "/tmp/a/",
		SecretKey:    "cluster-secret",
		SessionToken: "cluster-token",
	}}
	if err := f.Encrypt(); err != nil {
		t.Fatalf("encrypt: %s", err)
	}
	if !f.KeyEncrypted {
		t.Fatal("format must be encrypted")
	}
	cipherClusterSecret := f.ObjectClusters[0].SecretKey

	// mount-style: the cluster entry comes straight from the encrypted
	// format, so its credentials are still ciphertext and clusterFor must
	// decrypt the whole view
	oc := f.ObjectClusters[0]
	cf, err := clusterFor(f, &oc)
	if err != nil {
		t.Fatalf("clusterFor: %s", err)
	}
	if cf.KeyEncrypted {
		t.Fatal("clusterFor must return a decrypted view")
	}
	if cf.SecretKey != "cluster-secret" || cf.SessionToken != "cluster-token" {
		t.Fatalf("cluster credentials not decrypted: %q / %q", cf.SecretKey, cf.SessionToken)
	}
	if cf.Bucket != "/tmp/a/" || cf.Storage != "file" {
		t.Fatalf("cluster view not overridden: %s %s", cf.Storage, cf.Bucket)
	}
	// the in-place decrypt must not leak into the caller's format
	if f.ObjectClusters[0].SecretKey != cipherClusterSecret {
		t.Fatalf("clusterFor mutated the caller's registry: %q", f.ObjectClusters[0].SecretKey)
	}

	// config-style: the caller decrypted the format beforehand, so the
	// plaintext credentials from the CLI must pass through untouched
	plain := f.Clone()
	if err := plain.Decrypt(); err != nil {
		t.Fatalf("decrypt: %s", err)
	}
	cf2, err := clusterFor(*plain, &meta.ObjectCluster{
		ID: 2, Storage: "file", Bucket: "/tmp/b/", AccessKey: "ak", SecretKey: "plaintext-sk",
	})
	if err != nil {
		t.Fatalf("clusterFor on decrypted format: %s", err)
	}
	if cf2.SecretKey != "plaintext-sk" || cf2.AccessKey != "ak" || cf2.Bucket != "/tmp/b/" {
		t.Fatalf("plaintext credentials altered: %+v", cf2)
	}
}
