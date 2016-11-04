package pglogrus

import (
	"database/sql"
	"encoding/json"
	"sync"

	"github.com/Sirupsen/logrus"
)

// Set pglogrus.BufSize = <value> _before_ calling NewHook
// Once the buffer is full, logging will start blocking, waiting for slots to
// be available in the queue.
var BufSize uint = 8192

var InsertFunc = func(db *sql.DB, entry *logrus.Entry) error {
	jsonData, err := json.Marshal(entry.Data)
	if err != nil {
		return err
	}

	_, err = db.Exec("INSERT INTO logs(level, message, message_data, created_at) VALUES ($1,$2,$3,$4);", entry.Level, entry.Message, jsonData, entry.Time)
	return err
}

// Hook to send logs to a PostgreSQL database
type Hook struct {
	Extra       map[string]interface{}
	db          *sql.DB
	buf         chan *logrus.Entry
	wg          sync.WaitGroup
	mu          sync.RWMutex
	synchronous bool
	blacklist   map[string]bool
	insertFunc  func(*sql.DB, *logrus.Entry) error
}

// NewHook creates a PGHook to be added to an instance of logger.
func NewHook(db *sql.DB, extra map[string]interface{}) *Hook {
	return &Hook{
		Extra:       extra,
		db:          db,
		buf:         make(chan *logrus.Entry, BufSize),
		synchronous: true,
		insertFunc:  InsertFunc,
		blacklist:   make(map[string]bool),
	}
}

// NewAsyncHook creates a hook to be added to an instance of logger.
// The hook created will be asynchronous, and it's the responsibility of the user to call the Flush method
// before exiting to empty the log queue.
func NewAsyncHook(db *sql.DB, extra map[string]interface{}) *Hook {
	hook := NewHook(db, extra)
	hook.synchronous = false
	go hook.fire() // Log in background
	return hook
}

// Fire is called when a log event is fired.
// We assume the entry will be altered by another hook,
// otherwise we might logging something wrong to PostgreSQL
func (hook *Hook) Fire(entry *logrus.Entry) error {
	hook.mu.RLock() // Claim the mutex as a RLock - allowing multiple go routines to log simultaneously
	defer hook.mu.RUnlock()

	// Don't modify entry.Data directly, as the entry will used after this hook was fired
	data := map[string]interface{}{}

	// Merge extra fields
	for k, v := range hook.Extra {
		data[k] = v
	}
	for k, v := range entry.Data {
		if !hook.blacklist[k] {
			data[k] = v
			if k == logrus.ErrorKey {
				asError, isError := v.(error)
				_, isMarshaler := v.(json.Marshaler)
				if isError && !isMarshaler {
					data[k] = newMarshalableError(asError)
				}
			}
		}
	}

	newEntry := &logrus.Entry{
		Logger:  entry.Logger,
		Data:    data,
		Time:    entry.Time,
		Level:   entry.Level,
		Message: entry.Message,
	}

	if hook.synchronous {
		return hook.insertFunc(hook.db, newEntry)
	}

	hook.wg.Add(1)
	hook.buf <- newEntry
	return nil
}

// Flush waits for the log queue to be empty.
// This func is meant to be used when the hook was created with NewAsyncHook.
func (hook *Hook) Flush() {
	if hook.synchronous {
		return // nothing left to do
	}

	hook.mu.Lock() // claim the mutex as a Lock - we want exclusive access to it
	defer hook.mu.Unlock()

	hook.wg.Wait()
}

// fire will loop on the 'buf' channel, and write entries to pg
func (hook *Hook) fire() {
	for {
		entry := <-hook.buf // receive new entry on channel
		hook.insertFunc(hook.db, entry)
		hook.wg.Done()
	}
}

// Levels returns the available logging levels.
func (hook *Hook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.FatalLevel,
		logrus.ErrorLevel,
		logrus.WarnLevel,
		logrus.InfoLevel,
		logrus.DebugLevel,
	}
}

// Blacklist creates a blacklist map to filter some message keys.
// This useful when you want your application to log extra fields locally
// but don't want pg to store them.
func (hook *Hook) Blacklist(b []string) {
	hook.mu.Lock()
	defer hook.mu.Unlock()
	for _, elem := range b {
		hook.blacklist[elem] = true
	}
}
