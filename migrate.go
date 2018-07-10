// Package migrate reads migrations from sources and runs them against databases.
// Sources are defined by the `source.Driver` and databases by the `database.Driver`
// interface. The driver interfaces are kept "dump", all migration logic is kept
// in this package.
package migrate

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/basekit/migrate/database"
	"github.com/basekit/migrate/source"
)

// DefaultPrefetchMigrations sets the number of migrations to pre-read
// from the source. This is helpful if the source is remote, but has little
// effect for a local source (i.e. file system).
// Please note that this setting has a major impact on the memory usage,
// since each pre-read migration is buffered in memory. See DefaultBufferSize.
var DefaultPrefetchMigrations = uint(10)

// DefaultLockTimeout sets the max time a database driver has to acquire a lock.
var DefaultLockTimeout = 15 * time.Second

var (
	ErrNoChange    		= fmt.Errorf("no change")
	ErrNilVersion  		= fmt.Errorf("no migration")
	ErrLocked      		= fmt.Errorf("database locked")
	ErrLockTimeout 		= fmt.Errorf("timeout: can't acquire database lock")
	ErrGetAllVersions 	= fmt.Errorf("all versions not available")
)

// ErrShortLimit is an error returned when not enough migrations
// can be returned by a source for a given limit.
type ErrShortLimit struct {
	Short uint
}

// Error implements the error interface.
func (e ErrShortLimit) Error() string {
	return fmt.Sprintf("limit %v short", e.Short)
}

type ErrDirty struct {
	Version int
}

func (e ErrDirty) Error() string {
	return fmt.Sprintf("Dirty database version %v. Fix and force version.", e.Version)
}

type Migrate struct {
	sourceName   string
	sourceDrv    source.Driver
	databaseName string
	databaseDrv  database.Driver

	// Log accepts a Logger interface
	Log Logger

	// GracefulStop accepts `true` and will stop executing migrations
	// as soon as possible at a safe break point, so that the database
	// is not corrupted.
	GracefulStop   chan bool
	isGracefulStop bool

	isLockedMu *sync.Mutex
	isLocked   bool

	// PrefetchMigrations defaults to DefaultPrefetchMigrations,
	// but can be set per Migrate instance.
	PrefetchMigrations uint



	// LockTimeout defaults to DefaultLockTimeout,
	// but can be set per Migrate instance.
	LockTimeout time.Duration
}

// New returns a new Migrate instance from a source URL and a database URL.
// The URL scheme is defined by each driver.
func New(sourceUrl, databaseUrl string) (*Migrate, error) {
	m := newCommon()

	sourceName, err := schemeFromUrl(sourceUrl)
	if err != nil {
		return nil, err
	}
	m.sourceName = sourceName

	databaseName, err := schemeFromUrl(databaseUrl)
	if err != nil {
		return nil, err
	}
	m.databaseName = databaseName

	sourceDrv, err := source.Open(sourceUrl)
	if err != nil {
		return nil, err
	}
	m.sourceDrv = sourceDrv

	databaseDrv, err := database.Open(databaseUrl)
	if err != nil {
		return nil, err
	}
	m.databaseDrv = databaseDrv

	return m, nil
}

// NewWithDatabaseInstance returns a new Migrate instance from a source URL
// and an existing database instance. The source URL scheme is defined by each driver.
// Use any string that can serve as an identifier during logging as databaseName.
// You are responsible for closing the underlying database client if necessary.
func NewWithDatabaseInstance(sourceUrl string, databaseName string, databaseInstance database.Driver) (*Migrate, error) {
	m := newCommon()

	sourceName, err := schemeFromUrl(sourceUrl)
	if err != nil {
		return nil, err
	}
	m.sourceName = sourceName

	m.databaseName = databaseName

	sourceDrv, err := source.Open(sourceUrl)
	if err != nil {
		return nil, err
	}
	m.sourceDrv = sourceDrv

	m.databaseDrv = databaseInstance

	return m, nil
}

// NewWithSourceInstance returns a new Migrate instance from an existing source instance
// and a database URL. The database URL scheme is defined by each driver.
// Use any string that can serve as an identifier during logging as sourceName.
// You are responsible for closing the underlying source client if necessary.
func NewWithSourceInstance(sourceName string, sourceInstance source.Driver, databaseUrl string) (*Migrate, error) {
	m := newCommon()

	databaseName, err := schemeFromUrl(databaseUrl)
	if err != nil {
		return nil, err
	}
	m.databaseName = databaseName

	m.sourceName = sourceName

	databaseDrv, err := database.Open(databaseUrl)
	if err != nil {
		return nil, err
	}
	m.databaseDrv = databaseDrv

	m.sourceDrv = sourceInstance

	return m, nil
}

// NewWithInstance returns a new Migrate instance from an existing source and
// database instance. Use any string that can serve as an identifier during logging
// as sourceName and databaseName. You are responsible for closing down
// the underlying source and database client if necessary.
func NewWithInstance(sourceName string, sourceInstance source.Driver, databaseName string, databaseInstance database.Driver) (*Migrate, error) {
	m := newCommon()

	m.sourceName = sourceName
	m.databaseName = databaseName

	m.sourceDrv = sourceInstance
	m.databaseDrv = databaseInstance

	return m, nil
}

func newCommon() *Migrate {
	return &Migrate{
		GracefulStop:       make(chan bool, 1),
		PrefetchMigrations: DefaultPrefetchMigrations,
		LockTimeout:        DefaultLockTimeout,
		isLockedMu:         &sync.Mutex{},
	}
}

// Close closes the the source and the database.
func (m *Migrate) Close() (source error, database error) {
	databaseSrvClose := make(chan error)
	sourceSrvClose := make(chan error)

	m.logVerbosePrintf("Closing source and database\n")

	go func() {
		databaseSrvClose <- m.databaseDrv.Close()
	}()

	go func() {
		sourceSrvClose <- m.sourceDrv.Close()
	}()

	return <-sourceSrvClose, <-databaseSrvClose
}

// Migrate looks at the currently active migration version,
// then migrates either up or down to the specified version.
func (m *Migrate) Migrate(version uint) error {
	if err := m.lock(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	go m.read(curVersion, int(version), ret)

	return m.unlockErr(m.runMigrations(ret))
}

// Steps looks at the currently active migration version.
// It will migrate up if n > 0, and down if n < 0.
func (m *Migrate) Steps(n int) error {
	if n == 0 {
		return ErrNoChange
	}

	if err := m.lock(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)

	if n > 0 {
		go m.readUp(curVersion, n, ret, false)
	} else {
		go m.readDown(curVersion, -n, ret)
	}

	return m.unlockErr(m.runMigrations(ret))
}

// Up looks at the currently active migration version
// and will migrate all the way up (applying all up migrations).
func (m *Migrate) Up(includeMissing bool) error {
	if err := m.lock(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()

	if includeMissing == true && curVersion >= 0 {
		allVersions, err := m.databaseDrv.GetAllVersions()
		if err != nil {
			return m.unlockErr(err)
		}

		if (len(allVersions) > 0) {
			sortedKeys := make([]int, 0, len(allVersions))
			for k := range allVersions {
				sortedKeys = append(sortedKeys, k)
			}
			sort.Ints(sortedKeys)
			for _, v := range sortedKeys {
				curVersion = v
				dirty = allVersions[v]
				break
			}
		}
	}

	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)

	go m.readUp(curVersion, -1, ret, includeMissing)
	return m.unlockErr(m.runMigrations(ret))
}

// Down looks at the currently active migration version
// and will migrate all the way down (applying all down migrations).
func (m *Migrate) Down() error {
	if err := m.lock(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	go m.readDown(curVersion, -1, ret)
	return m.unlockErr(m.runMigrations(ret))
}

// Drop deletes everything in the database.
func (m *Migrate) Drop() error {
	if err := m.lock(); err != nil {
		return err
	}
	if err := m.databaseDrv.Drop(); err != nil {
		return m.unlockErr(err)
	}
	return m.unlock()
}

// Run runs any migration provided by you against the database.
// It does not check any currently active version in database.
// Usually you don't need this function at all. Use Migrate,
// Steps, Up or Down instead.
func (m *Migrate) Run(migration ...*Migration) error {
	if len(migration) == 0 {
		return ErrNoChange
	}

	if err := m.lock(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)

	go func() {
		defer close(ret)
		for _, migr := range migration {
			if m.PrefetchMigrations > 0 && migr.Body != nil {
				m.logVerbosePrintf("Start buffering %v\n", migr.LogString())
			} else {
				m.logVerbosePrintf("Scheduled %v\n", migr.LogString())
			}

			ret <- migr
			go migr.Buffer()
		}
	}()

	return m.unlockErr(m.runMigrations(ret))
}

// Force sets a migration version.
// It does not check any currently active version in database.
// It resets the dirty state to false.
func (m *Migrate) Force(version int) error {
	if version < -1 {
		panic("version must be >= -1")
	}

	if err := m.lock(); err != nil {
		return err
	}

	if err := m.databaseDrv.SetVersion(version, false); err != nil {
		return m.unlockErr(err)
	}

	return m.unlock()
}

// Version returns the currently active migration version.
// If no migration has been applied, yet, it will return ErrNilVersion.
func (m *Migrate) Version() (version uint, dirty bool, err error) {
	v, d, err := m.databaseDrv.Version()
	if err != nil {
		return 0, false, err
	}

	if v == database.NilVersion {
		return 0, false, ErrNilVersion
	}

	return suint(v), d, nil
}

// read reads either up or down migrations from source `from` to `to`.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once read is done reading it will close the ret channel.
func (m *Migrate) read(from int, to int, ret chan<- interface{}) {
	defer close(ret)

	// check if from version exists
	if from >= 0 {
		if m.versionExists(suint(from)) != nil {
			ret <- os.ErrNotExist
			return
		}
	}

	// check if to version exists
	if to >= 0 {
		if m.versionExists(suint(to)) != nil {
			ret <- os.ErrNotExist
			return
		}
	}

	// no change?
	if from == to {
		ret <- ErrNoChange
		return
	}

	if from < to {
		// it's going up
		// apply first migration if from is nil version
		if from == -1 {
			firstVersion, err := m.sourceDrv.First()
			if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(firstVersion, int(firstVersion))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go migr.Buffer()
			from = int(firstVersion)
		}

		// run until we reach target ...
		for from < to {
			if m.stop() {
				return
			}

			next, err := m.sourceDrv.Next(suint(from))
			if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(next, int(next))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go migr.Buffer()
			from = int(next)
		}

	} else {
		// it's going down
		// run until we reach target ...
		for from > to && from >= 0 {
			if m.stop() {
				return
			}

			prev, err := m.sourceDrv.Prev(suint(from))
			if os.IsNotExist(err) && to == -1 {
				// apply nil migration
				migr, err := m.newMigration(suint(from), -1)
				if err != nil {
					ret <- err
					return
				}
				ret <- migr
				go migr.Buffer()
				return

			} else if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(suint(from), int(prev))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go migr.Buffer()
			from = int(prev)
		}
	}
}

// readUp reads up migrations from `from` limitted by `limit`.
// limit can be -1, implying no limit and reading until there are no more migrations.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once readUp is done reading it will close the ret channel.
func (m *Migrate) readUp(from int, limit int, ret chan<- interface{}, includeMissing bool) {
	defer close(ret)

	// check if from version exists
	if from >= 0 {
		if m.versionExists(suint(from)) != nil {
			ret <- os.ErrNotExist
			return
		}
	}

	if limit == 0 {
		ret <- ErrNoChange
		return
	}

	allVersions := make(map[int]bool)
	var err error
	versionCheck := false;
	if includeMissing == true {
		allVersions, err = m.databaseDrv.GetAllVersions()
		if err != nil {
			ret <- ErrGetAllVersions
			return
		}
		if len(allVersions) > 0 {
			versionCheck = true;
		}
	}

	count := 0
	for count < limit || limit == -1 {
		if m.stop() {
			return
		}

		// apply first migration if from is nil version
		if from == -1 {
			firstVersion, err := m.sourceDrv.First()
			if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(firstVersion, int(firstVersion))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go migr.Buffer()
			from = int(firstVersion)
			count++
			continue
		}

		// apply next migration
		next, err := m.sourceDrv.Next(suint(from))
		if versionCheck == true {
			if _, ok := allVersions[int(next)]; ok {
				from = int(next)
				continue
			}
		}

		if os.IsNotExist(err) {
			// no limit, but no migrations applied?
			if limit == -1 && count == 0 {
				ret <- ErrNoChange
				return
			}

			// no limit, reached end
			if limit == -1 {
				return
			}

			// reached end, and didn't apply any migrations
			if limit > 0 && count == 0 {
				ret <- os.ErrNotExist
				return
			}

			// applied less migrations than limit?
			if count < limit {
				ret <- ErrShortLimit{suint(limit - count)}
				return
			}
		}
		if err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(next, int(next))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()
		from = int(next)
		count++
	}
}

// readDown reads down migrations from `from` limitted by `limit`.
// limit can be -1, implying no limit and reading until there are no more migrations.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once readDown is done reading it will close the ret channel.
func (m *Migrate) readDown(from int, limit int, ret chan<- interface{}) {
	defer close(ret)

	// check if from version exists
	if from >= 0 {
		if m.versionExists(suint(from)) != nil {
			ret <- os.ErrNotExist
			return
		}
	}

	if limit == 0 {
		ret <- ErrNoChange
		return
	}

	// no change if already at nil version
	if from == -1 && limit == -1 {
		ret <- ErrNoChange
		return
	}

	// can't go over limit if already at nil version
	if from == -1 && limit > 0 {
		ret <- os.ErrNotExist
		return
	}

	count := 0
	for count < limit || limit == -1 {
		if m.stop() {
			return
		}

		prev, err := m.sourceDrv.Prev(suint(from))
		if os.IsNotExist(err) {
			// no limit or haven't reached limit, apply "first" migration
			if limit == -1 || limit-count > 0 {
				firstVersion, err := m.sourceDrv.First()
				if err != nil {
					ret <- err
					return
				}

				migr, err := m.newMigration(firstVersion, -1)
				if err != nil {
					ret <- err
					return
				}
				ret <- migr
				go migr.Buffer()
				count++
			}

			if count < limit {
				ret <- ErrShortLimit{suint(limit - count)}
			}
			return
		}
		if err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(suint(from), int(prev))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()
		from = int(prev)
		count++
	}
}

// runMigrations reads *Migration and error from a channel. Any other type
// sent on this channel will result in a panic. Each migration is then
// proxied to the database driver and run against the database.
// Before running a newly received migration it will check if it's supposed
// to stop execution because it might have received a stop signal on the
// GracefulStop channel.
func (m *Migrate) runMigrations(ret <-chan interface{}) error {
	for r := range ret {

		if m.stop() {
			return nil
		}

		switch r.(type) {
		case error:
			return r.(error)

		case *Migration:
			migr := r.(*Migration)

			// set version with dirty state
			if err := m.databaseDrv.SetVersion(migr.TargetVersion, true); err != nil {
				return err
			}

			if migr.Body != nil {
				m.logVerbosePrintf("Read and execute %v\n", migr.LogString())
				if err := m.databaseDrv.Run(migr.BufferedBody); err != nil {
					return err
				}
			}

			// set clean state
			if err := m.databaseDrv.SetVersion(migr.TargetVersion, false); err != nil {
				return err
			}

			endTime := time.Now()
			readTime := migr.FinishedReading.Sub(migr.StartedBuffering)
			runTime := endTime.Sub(migr.FinishedReading)

			// log either verbose or normal
			if m.Log != nil {
				if m.Log.Verbose() {
					m.logPrintf("Finished %v (read %v, ran %v)\n", migr.LogString(), readTime, runTime)
				} else {
					m.logPrintf("%v (%v)\n", migr.LogString(), readTime+runTime)
				}
			}

		default:
			panic("unknown type")
		}
	}
	return nil
}

// versionExists checks the source if either the up or down migration for
// the specified migration version exists.
func (m *Migrate) versionExists(version uint) error {
	// try up migration first
	up, _, err := m.sourceDrv.ReadUp(version)
	if err == nil {
		defer up.Close()
	}
	if os.IsExist(err) {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	// then try down migration
	down, _, err := m.sourceDrv.ReadDown(version)
	if err == nil {
		defer down.Close()
	}
	if os.IsExist(err) {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	return os.ErrNotExist
}

// stop returns true if no more migrations should be run against the database
// because a stop signal was received on the GracefulStop channel.
// Calls are cheap and this function is not blocking.
func (m *Migrate) stop() bool {
	if m.isGracefulStop {
		return true
	}

	select {
	case <-m.GracefulStop:
		m.isGracefulStop = true
		return true

	default:
		return false
	}
}

// newMigration is a helper func that returns a *Migration for the
// specified version and targetVersion.
func (m *Migrate) newMigration(version uint, targetVersion int) (*Migration, error) {
	var migr *Migration

	if targetVersion >= int(version) {
		r, identifier, err := m.sourceDrv.ReadUp(version)
		if os.IsNotExist(err) {
			// create "empty" migration
			migr, err = NewMigration(nil, "", version, targetVersion)
			if err != nil {
				return nil, err
			}

		} else if err != nil {
			return nil, err

		} else {
			// create migration from up source
			migr, err = NewMigration(r, identifier, version, targetVersion)
			if err != nil {
				return nil, err
			}
		}

	} else {
		r, identifier, err := m.sourceDrv.ReadDown(version)
		if os.IsNotExist(err) {
			// create "empty" migration
			migr, err = NewMigration(nil, "", version, targetVersion)
			if err != nil {
				return nil, err
			}

		} else if err != nil {
			return nil, err

		} else {
			// create migration from down source
			migr, err = NewMigration(r, identifier, version, targetVersion)
			if err != nil {
				return nil, err
			}
		}
	}

	if m.PrefetchMigrations > 0 && migr.Body != nil {
		m.logVerbosePrintf("Start buffering %v\n", migr.LogString())
	} else {
		m.logVerbosePrintf("Scheduled %v\n", migr.LogString())
	}

	return migr, nil
}

// lock is a thread safe helper function to lock the database.
// It should be called as late as possible when running migrations.
func (m *Migrate) lock() error {
	m.isLockedMu.Lock()
	defer m.isLockedMu.Unlock()

	if m.isLocked {
		return ErrLocked
	}

	// create done channel, used in the timeout goroutine
	done := make(chan bool, 1)
	defer func() {
		done <- true
	}()

	// use errchan to signal error back to this context
	errchan := make(chan error, 2)

	// start timeout goroutine
	timeout := time.After(m.LockTimeout)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-timeout:
				errchan <- ErrLockTimeout
				return
			}
		}
	}()

	// now try to acquire the lock
	go func() {
		if err := m.databaseDrv.Lock(); err != nil {
			errchan <- err
		} else {
			errchan <- nil
		}
		return
	}()

	// wait until we either recieve ErrLockTimeout or error from Lock operation
	err := <-errchan
	if err == nil {
		m.isLocked = true
	}
	return err
}

// unlock is a thread safe helper function to unlock the database.
// It should be called as early as possible when no more migrations are
// expected to be executed.
func (m *Migrate) unlock() error {
	m.isLockedMu.Lock()
	defer m.isLockedMu.Unlock()

	if err := m.databaseDrv.Unlock(); err != nil {
		// BUG: Can potentially create a deadlock. Add a timeout.
		return err
	}

	m.isLocked = false
	return nil
}

// unlockErr calls unlock and returns a combined error
// if a prevErr is not nil.
func (m *Migrate) unlockErr(prevErr error) error {
	if err := m.unlock(); err != nil {
		return NewMultiError(prevErr, err)
	}
	return prevErr
}

// logPrintf writes to m.Log if not nil
func (m *Migrate) logPrintf(format string, v ...interface{}) {
	if m.Log != nil {
		m.Log.Printf(format, v...)
	}
}

// logVerbosePrintf writes to m.Log if not nil. Use for verbose logging output.
func (m *Migrate) logVerbosePrintf(format string, v ...interface{}) {
	if m.Log != nil && m.Log.Verbose() {
		m.Log.Printf(format, v...)
	}
}
