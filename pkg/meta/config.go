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

package meta

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/emmansun/gmsm/sm3"
	"github.com/emmansun/gmsm/sm4"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/version"
	"github.com/pkg/errors"
)

// Config for clients.
type Config struct {
	Retries            int
	MaxDeletes         int
	SkipDirNlink       int
	CaseInsensi        bool
	ReadOnly           bool
	NoBGJob            bool // disable background jobs like clean-up, backup, etc.
	OpenCache          time.Duration
	OpenCacheLimit     uint64 // max number of files to cache (soft limit)
	Heartbeat          time.Duration
	MountPoint         string
	Subdir             string
	AtimeMode          string
	DirStatFlushPeriod time.Duration
	SkipDirMtime       time.Duration
	Sid                uint64
	SortDir            bool
	FastStatfs         bool
	NetworkInterfaces  []string // list of network interfaces to use for IP discovery (empty means all)
}

func DefaultConf() *Config {
	return &Config{Retries: 10, MaxDeletes: 2, Heartbeat: 12 * time.Second, AtimeMode: NoAtime, DirStatFlushPeriod: 1 * time.Second}
}

func (c *Config) SelfCheck() {
	if c.MaxDeletes == 0 {
		logger.Warnf("Deleting object will be disabled since max-deletes is 0")
	}
	if c.Heartbeat != 0 && c.Heartbeat < time.Second {
		logger.Warnf("heartbeat should not be less than 1 second")
		c.Heartbeat = time.Second
	}
	if c.Heartbeat > time.Minute*10 {
		logger.Warnf("heartbeat should not be greater than 10 minutes")
		c.Heartbeat = time.Minute * 10
	}
}

type Format struct {
	Name              string
	UUID              string
	Storage           string
	StorageClass      string `json:",omitempty"`
	Tiers             object.Tiers
	Bucket            string
	AccessKey         string `json:",omitempty"`
	SecretKey         string `json:",omitempty"`
	SessionToken      string `json:",omitempty"`
	BlockSize         int
	Compression       string `json:",omitempty"`
	Shards            int    `json:",omitempty"`
	HashPrefix        bool   `json:",omitempty"`
	Capacity          uint64 `json:",omitempty"`
	Inodes            uint64 `json:",omitempty"`
	EncryptKey        string `json:",omitempty"`
	EncryptAlgo       string `json:",omitempty"`
	KeyEncrypted      bool   `json:",omitempty"`
	UploadLimit       int64  `json:",omitempty"` // Mbps
	DownloadLimit     int64  `json:",omitempty"` // Mbps
	TrashDays         int
	MetaVersion       int    `json:",omitempty"`
	MinClientVersion  string `json:",omitempty"`
	MaxClientVersion  string `json:",omitempty"`
	DirStats          bool   `json:",omitempty"`
	UserGroupQuota    bool   `json:",omitempty"`
	EnableACL         bool
	RangerRestUrl     string `json:",omitempty"`
	RangerService     string `json:",omitempty"`
	ChangeLog         bool   `json:",omitempty"`
	ChangeLogMaxAge   int64  `json:",omitempty"`
	ChangeLogMaxLines int64  `json:",omitempty"`

	// ObjectClusters is the registry of EXTRA object storage sub-clusters.
	// Cluster 0 is implicit and always mirrors the top-level Storage/Bucket
	// fields; entries here carry contiguous ids starting from 1. The
	// registry is append-only: existing entries can never be modified.
	ObjectClusters []ObjectCluster `json:",omitempty"`

	//kerberos
	KerbConf string `json:",omitempty"`
}

// ObjectCluster describes one object storage sub-cluster. File data of an
// inode is routed to the cluster recorded in the inode's objectID bits.
type ObjectCluster struct {
	ID           uint8
	Storage      string
	StorageClass string `json:",omitempty"`
	Bucket       string
	AccessKey    string `json:",omitempty"`
	SecretKey    string `json:",omitempty"`
	SessionToken string `json:",omitempty"`
}

// ObjectClusterByID returns the registered cluster with the given id.
// Cluster 0 is always the top-level storage config; ids >= 1 come from the
// registry. It returns nil if the id is not registered.
func (f *Format) ObjectClusterByID(id uint8) *ObjectCluster {
	if id == 0 {
		return &ObjectCluster{
			ID:           0,
			Storage:      f.Storage,
			StorageClass: f.StorageClass,
			Bucket:       f.Bucket,
			AccessKey:    f.AccessKey,
			SecretKey:    f.SecretKey,
			SessionToken: f.SessionToken,
		}
	}
	if int(id) <= len(f.ObjectClusters) && f.ObjectClusters[id-1].ID == id {
		return &f.ObjectClusters[id-1]
	}
	return nil
}

// Clone returns a deep copy of the format.
func (f *Format) Clone() *Format {
	c := *f
	if f.Tiers != nil {
		c.Tiers = make(object.Tiers, len(f.Tiers))
		for id, t := range f.Tiers {
			c.Tiers[id] = t
		}
	}
	if f.ObjectClusters != nil {
		c.ObjectClusters = make([]ObjectCluster, len(f.ObjectClusters))
		copy(c.ObjectClusters, f.ObjectClusters)
	}
	return &c
}

func (f *Format) update(old *Format, force bool) error {
	if err := f.validateObjectClusters(); err != nil {
		return err
	}
	if force {
		logger.Warnf("Existing volume will be overwrited: %s", old)
	} else {
		if !f.objectClustersAppendOnly(old) {
			return fmt.Errorf("existing object clusters cannot be modified; only appending new clusters is allowed")
		}
		var args []interface{}
		switch {
		case f.Name != old.Name:
			args = []interface{}{"name", old.Name, f.Name}
		case f.BlockSize != old.BlockSize:
			args = []interface{}{"block size", old.BlockSize, f.BlockSize}
		case f.Compression != old.Compression:
			args = []interface{}{"compression", old.Compression, f.Compression}
		case f.Shards != old.Shards:
			args = []interface{}{"shards", old.Shards, f.Shards}
		case f.HashPrefix != old.HashPrefix:
			args = []interface{}{"hash prefix", old.HashPrefix, f.HashPrefix}
		case f.MetaVersion != old.MetaVersion:
			args = []interface{}{"meta version", old.MetaVersion, f.MetaVersion}
		}
		if args == nil {
			if f.UUID != old.UUID {
				if err := f.Decrypt(); err != nil {
					return fmt.Errorf("decrypt format: %s", err)
				}
				f.UUID = old.UUID // UUID cannot be changed alone
				if err := f.Encrypt(); err != nil {
					return fmt.Errorf("encrypt format: %s", err)
				}
			}
		} else {
			return fmt.Errorf("cannot update volume %s from %v to %v", args...)
		}
	}
	return nil
}

// validateObjectClusters checks that the registry is well-formed:
// ids are contiguous from 1 (0 is the implicit top-level cluster), and the
// total count does not exceed the 8-bit object id limit.
func (f *Format) validateObjectClusters() error {
	if len(f.ObjectClusters) > int(MaxObjectID) {
		return fmt.Errorf("too many object clusters: %d; max %d", len(f.ObjectClusters), MaxObjectID)
	}
	for i, c := range f.ObjectClusters {
		if c.ID != uint8(i+1) {
			return fmt.Errorf("object cluster ids must be contiguous from 1: index %d has id %d", i, c.ID)
		}
	}
	return nil
}

// objectClustersAppendOnly reports whether new is old plus appended
// entries only: every entry present in old must be unchanged. Secrets are
// compared after decryption because re-encrypting a format applies a fresh
// nonce, which would otherwise make unchanged entries look modified.
func (f *Format) objectClustersAppendOnly(old *Format) bool {
	if len(f.ObjectClusters) < len(old.ObjectClusters) {
		return false
	}
	newClusters, oldClusters := f.ObjectClusters, old.ObjectClusters
	if f.KeyEncrypted || old.KeyEncrypted {
		nf, of := *f, *old
		nf.ObjectClusters = append([]ObjectCluster(nil), f.ObjectClusters...)
		of.ObjectClusters = append([]ObjectCluster(nil), old.ObjectClusters...)
		if nf.Decrypt() != nil || of.Decrypt() != nil {
			return false
		}
		newClusters, oldClusters = nf.ObjectClusters, of.ObjectClusters
	}
	for i, c := range oldClusters {
		if newClusters[i] != c {
			return false
		}
	}
	return true
}

func (f *Format) RemoveSecret() {
	if f.SecretKey != "" {
		f.SecretKey = "removed"
	}
	if f.SessionToken != "" {
		f.SessionToken = "removed"
	}
	if f.EncryptKey != "" {
		f.EncryptKey = "removed"
	}
	for i := range f.ObjectClusters {
		if f.ObjectClusters[i].SecretKey != "" {
			f.ObjectClusters[i].SecretKey = "removed"
		}
		if f.ObjectClusters[i].SessionToken != "" {
			f.ObjectClusters[i].SessionToken = "removed"
		}
	}
}

func (f *Format) String() string {
	t := f.Clone()
	t.RemoveSecret()
	s, _ := json.MarshalIndent(t, "", "  ")
	return string(s)
}

func (f *Format) CheckVersion() error {
	if f.MetaVersion > MaxVersion {
		return fmt.Errorf("incompatible metadata version: %d; please upgrade the client", f.MetaVersion)
	}

	ver := version.GetVersion()
	return f.CheckCliVersion(&ver)
}

func (f *Format) CheckCliVersion(ver *version.Semver) error {
	if ver == nil {
		return errors.New("version is nil")
	}

	if f.MinClientVersion != "" {
		minClientVer := version.Parse(f.MinClientVersion)
		r, err := version.CompareVersions(ver, minClientVer)
		if err == nil && r < 0 {
			err = fmt.Errorf("allowed minimum version: %s; please upgrade the client", f.MinClientVersion)
		}
		if err != nil {
			return err
		}
	}
	if f.MaxClientVersion != "" {
		maxClientVer := version.Parse(f.MaxClientVersion)
		r, err := version.CompareVersions(ver, maxClientVer)
		if err == nil && r > 0 {
			err = fmt.Errorf("allowed maximum version: %s; please use an older client", f.MaxClientVersion)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func newCipher(algo string, key string) (cipher.AEAD, error) {
	switch algo {
	case object.SM4GCM:
		block, err := sm4.NewCipher(sm3.Kdf([]byte(key), 16))
		if err != nil {
			return nil, fmt.Errorf("new sm4 cipher: %s", err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("new sm4 GCM: %s", err)
		}
		return aead, nil
	default:
		hashKey := md5.Sum([]byte(key))
		block, err := aes.NewCipher(hashKey[:])
		if err != nil {
			return nil, fmt.Errorf("new cipher: %s", err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("new GCM: %s", err)
		}
		return aead, nil
	}
}

func (f *Format) Encrypt() error {
	if f.KeyEncrypted {
		return nil
	}
	hasSecret := f.SecretKey != "" || f.EncryptKey != "" || f.SessionToken != ""
	for i := range f.ObjectClusters {
		hasSecret = hasSecret || f.ObjectClusters[i].SecretKey != "" || f.ObjectClusters[i].SessionToken != ""
	}
	if !hasSecret {
		return nil
	}
	ci, err := newCipher(f.EncryptAlgo, f.UUID)
	if err != nil {
		return err
	}
	encrypt := func(k *string) {
		if *k == "" {
			return
		}
		nonce := make([]byte, ci.NonceSize())
		if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
			logger.Fatalf("generate nonce for secret key: %s", err)
		}
		ciphertext := ci.Seal(nil, nonce, []byte(*k), nil)
		buf := make([]byte, ci.NonceSize()+len(ciphertext))
		copy(buf, nonce)
		copy(buf[ci.NonceSize():], ciphertext)
		*k = base64.StdEncoding.EncodeToString(buf)
	}

	encrypt(&f.SecretKey)
	encrypt(&f.SessionToken)
	encrypt(&f.EncryptKey)
	for i := range f.ObjectClusters {
		encrypt(&f.ObjectClusters[i].SecretKey)
		encrypt(&f.ObjectClusters[i].SessionToken)
	}
	f.KeyEncrypted = true
	return nil
}

func (f *Format) Decrypt() error {
	if !f.KeyEncrypted {
		return nil
	}

	ci, err := newCipher(f.EncryptAlgo, f.UUID)
	if err != nil {
		return err
	}
	decrypt := func(k *string) {
		if *k == "" {
			return
		}
		if *k == "removed" {
			err = fmt.Errorf("secret was removed; please correct it with `config` command")
			return
		}
		buf, e := base64.StdEncoding.DecodeString(*k)
		if e != nil {
			err = fmt.Errorf("decode key: %s", e)
			return
		}
		plaintext, e := ci.Open(nil, buf[:ci.NonceSize()], buf[ci.NonceSize():], nil)
		if e != nil {
			err = fmt.Errorf("open cipher: %s", e)
			return
		}
		*k = string(plaintext)
	}

	decrypt(&f.EncryptKey)
	decrypt(&f.SecretKey)
	decrypt(&f.SessionToken)
	for i := range f.ObjectClusters {
		decrypt(&f.ObjectClusters[i].SecretKey)
		decrypt(&f.ObjectClusters[i].SessionToken)
	}
	f.KeyEncrypted = false
	return err
}
