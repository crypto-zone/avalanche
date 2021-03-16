// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package manager

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/database/leveldb"
	"github.com/ava-labs/avalanchego/database/memdb"
	"github.com/ava-labs/avalanchego/database/meterdb"
	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/version"
)

var (
	errNonSortedAndUniqueDBs = errors.New("managed databases were not sorted and unique")
)

type Manager interface {
	// Current returns the database with the current database version
	Current() *VersionedDatabase
	// Previous returns the database prior to the current database and true if a previous database exists.
	Previous() (*VersionedDatabase, bool)
	// GetDatabases returns all the managed databases in order from current to the oldest version
	GetDatabases() []*VersionedDatabase
	// Close all of the databases controlled by the manager
	Close() error

	// NewPrefixDBManager returns a new database manager with each of its databases
	// prefixed with [prefix]
	NewPrefixDBManager(prefix []byte) Manager
	// NewNestedPrefixDBManager returns a new database manager where each of its databases
	// has the nested prefix [prefix] applied to it.
	NewNestedPrefixDBManager(prefix []byte) Manager
	// NewMeterDBManager returns a new database manager with each of its databases
	// wrapped with a meterdb instance to support metrics on database performance.
	NewMeterDBManager(namespace string, registerer prometheus.Registerer) (Manager, error)
}

type manager struct {
	// databases with the current version at index 0 and prior versions in
	// descending order
	// invariant: len(databases) > 0
	databases []*VersionedDatabase
}

func (m *manager) Current() *VersionedDatabase { return m.databases[0] }

func (m *manager) Previous() (*VersionedDatabase, bool) {
	if len(m.databases) < 2 {
		return nil, false
	}
	return m.databases[1], true
}

func (m *manager) GetDatabases() []*VersionedDatabase { return m.databases }

func (m *manager) Close() error {
	errs := wrappers.Errs{}
	for _, db := range m.databases {
		errs.Add(db.Close())
	}

	return errs.Err
}

// wrapManager returns a new database manager with each managed database wrapped by
// the [wrap] function. If an error is returned by wrap, the error is returned
// immediately. If [wrap] never returns an error, then wrapManager is guaranteed to
// never return an error.
// the function wrap must return a database that can be closed without closing the
// underlying database.
func (m *manager) wrapManager(wrap func(db *VersionedDatabase) (*VersionedDatabase, error)) (*manager, error) {
	newManager := &manager{
		databases: make([]*VersionedDatabase, 0, len(m.databases)),
	}

	for _, db := range m.databases {
		wrappedDB, err := wrap(db)
		if err != nil {
			// ignore additional errors in favor of returning the original error
			_ = newManager.Close()
			return nil, err
		}
		newManager.databases = append(newManager.databases, wrappedDB)
	}
	return newManager, nil
}

// NewDefaultMemDBManager returns a database manager with a single memory db instance
// with a default version of v1.0.0
func NewDefaultMemDBManager() Manager {
	return &manager{
		databases: []*VersionedDatabase{
			{
				Database: memdb.New(),
				Version:  version.DefaultVersion1,
			},
		},
	}
}

// New creates a database manager at [filePath] by creating a database instance from each directory
// with a version <= [currentVersion]
func New(dbDirPath string, log logging.Logger, currentVersion version.Version) (Manager, error) {
	parser := version.NewDefaultParser()

	currentDBPath := path.Join(dbDirPath, currentVersion.String())
	currentDB, err := leveldb.New(currentDBPath, log, 0, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("couldn't create db at %s: %w", currentDBPath, err)
	}

	manager := &manager{
		databases: []*VersionedDatabase{
			{
				Database: currentDB,
				Version:  currentVersion,
			},
		},
	}
	err = filepath.Walk(dbDirPath, func(path string, info os.FileInfo, err error) error {
		// the walkFn is called with a non-nil error argument if an os.Lstat
		// or Readdirnames call returns an error. Both cases are considered
		// fatal in the traversal.
		// Reference: https://golang.org/pkg/path/filepath/#WalkFunc
		if err != nil {
			return err
		}
		// Skip the root directory
		if path == dbDirPath {
			return nil
		}

		// The database directory should only contain database directories, no files.
		if !info.IsDir() {
			return fmt.Errorf("unexpectedly found non-directory at %s", path)
		}
		_, dbName := filepath.Split(path)
		version, err := parser.Parse(dbName)
		if err != nil {
			return err
		}

		// If [version] is greater than or equal to the specified version
		// skip over creating the new database to avoid creating the same db
		// twice or creating a database with a version ahead of the desired one.
		if cmp := version.Compare(currentVersion); cmp >= 0 {
			return filepath.SkipDir
		}

		db, err := leveldb.New(path, log, 0, 0, 0)
		if err != nil {
			return fmt.Errorf("couldn't create db at %s: %w", path, err)
		}

		manager.databases = append(manager.databases, &VersionedDatabase{
			Database: db,
			Version:  version,
		})

		return filepath.SkipDir
	})
	SortDescending(manager.databases)

	// If an error occurred walking [dbDirPath] close the
	// database manager and return the original error here.
	if err != nil {
		_ = manager.Close()
		return nil, err
	}

	return manager, nil
}

// NewPrefixDBManager creates a new manager with each database instance prefixed
// by [prefix]
func (m *manager) NewPrefixDBManager(prefix []byte) Manager {
	m, _ = m.wrapManager(func(vdb *VersionedDatabase) (*VersionedDatabase, error) {
		return &VersionedDatabase{
			Database: prefixdb.New(prefix, vdb.Database),
			Version:  vdb.Version,
		}, nil
	})
	return m
}

// NewNestedPrefixDBManager creates a new manager with each database instance
// wrapped with a nested prfix of [prefix]
func (m *manager) NewNestedPrefixDBManager(prefix []byte) Manager {
	m, _ = m.wrapManager(func(vdb *VersionedDatabase) (*VersionedDatabase, error) {
		return &VersionedDatabase{
			Database: prefixdb.NewNested(prefix, vdb.Database),
			Version:  vdb.Version,
		}, nil
	})
	return m
}

// NewMeterDBManager wraps each database instance with a meterdb instance. The namespace
// is concatenated with the version of the database. Note: calling this more than once
// with the same [namespace] will cause a conflict error for the [registerer]
func (m *manager) NewMeterDBManager(namespace string, registerer prometheus.Registerer) (Manager, error) {
	return m.wrapManager(func(vdb *VersionedDatabase) (*VersionedDatabase, error) {
		mdb, err := meterdb.New(fmt.Sprintf("%s_%s", namespace, strings.ReplaceAll(vdb.Version.String(), ".", "_")), registerer, vdb.Database)
		if err != nil {
			return nil, err
		}
		return &VersionedDatabase{
			Database: mdb,
			Version:  vdb.Version,
		}, nil
	})
}

// NewManagerFromDBs
func NewManagerFromDBs(dbs []*VersionedDatabase) (Manager, error) {
	SortDescending(dbs)
	sortedAndUnique := utils.IsSortedAndUnique(innerSortDescendingVersionedDBs(dbs))
	if !sortedAndUnique {
		return nil, errNonSortedAndUniqueDBs
	}
	return &manager{
		databases: dbs,
	}, nil
}

type VersionedDatabase struct {
	database.Database
	version.Version
}

type innerSortDescendingVersionedDBs []*VersionedDatabase

// Less returns true if the version at index i is greater than the version at index j
// such that it will sort in descending order (newest version --> oldest version)
func (dbs innerSortDescendingVersionedDBs) Less(i, j int) bool {
	return dbs[i].Version.Compare(dbs[j].Version) > 0
}

func (dbs innerSortDescendingVersionedDBs) Len() int      { return len(dbs) }
func (dbs innerSortDescendingVersionedDBs) Swap(i, j int) { dbs[j], dbs[i] = dbs[i], dbs[j] }

func SortDescending(dbs []*VersionedDatabase) { sort.Sort(innerSortDescendingVersionedDBs(dbs)) }