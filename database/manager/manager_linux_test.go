// +build linux,amd64,rocksdballowed

// ^ Only build this file if this computer runs Linux AND is AMD64 AND rocksdb is allowed
// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package manager

import (
	"path"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ava-labs/avalanchego/database/rocksdb"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/version"
)

func TestNewSingleRocksDB(t *testing.T) {
	dir := t.TempDir()

	v1 := version.DefaultVersion1_0_0

	dbPath := path.Join(dir, v1.String())
	db, err := rocksdb.New(dbPath, logging.NoLog{})
	if err != nil {
		t.Fatal(err)
	}

	err = db.Close()
	if err != nil {
		t.Fatal(err)
	}

	manager, err := NewRocksDB(dir, logging.NoLog{}, v1)
	if err != nil {
		t.Fatal(err)
	}

	semDB := manager.Current()
	cmp := semDB.Version.Compare(v1)
	assert.Equal(t, 0, cmp, "incorrect version on current database")

	_, exists := manager.Previous()
	assert.False(t, exists, "there should be no previous database")

	dbs := manager.GetDatabases()
	assert.Len(t, dbs, 1)

	err = manager.Close()
	assert.NoError(t, err)
}
