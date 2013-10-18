package db

import (
	"bytes"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/couchbaselabs/walrus"

	"github.com/couchbaselabs/sync_gateway/base"
	"github.com/couchbaselabs/sync_gateway/channels"
)

//////// CHANGES WRITER

// Coordinates writing changes to channel-log documents. A singleton owned by a DatabaseContext.
type changesWriter struct {
	bucket     base.Bucket
	logWriters map[string]*channelLogWriter
	lock       sync.Mutex
}

// Creates a new changesWriter
func newChangesWriter(bucket base.Bucket) *changesWriter {
	return &changesWriter{bucket: bucket, logWriters: map[string]*channelLogWriter{}}
}

// Adds a change to all relevant logs, asynchronously.
func (c *changesWriter) addToChangeLogs(changedChannels base.Set, channelMap ChannelMap, entry channels.LogEntry, parentRevID string) error {
	var err error
	base.LogTo("Changes", "Updating #%d %q/%q in channels %s", entry.Sequence, entry.DocID, entry.RevID, changedChannels)
	for channel, removal := range channelMap {
		if removal != nil && removal.Seq != entry.Sequence {
			continue
		}
		// Set Removed flag appropriately for this channel:
		if removal != nil {
			entry.Flags |= channels.Removed
		} else {
			entry.Flags = entry.Flags &^ channels.Removed
		}
		c.addToChangeLog(channel, entry, parentRevID)
	}

	// Finally, add to the universal "*" channel.
	if EnableStarChannelLog {
		entry.Flags = entry.Flags &^ channels.Removed
		c.addToChangeLog("*", entry, parentRevID)
	}

	return err
}

// Blocks until all pending channel-log updates are complete.
func (c *changesWriter) checkpoint() {
	c.lock.Lock()
	defer c.lock.Unlock()
	for _, logWriter := range c.logWriters {
		logWriter.stop()
	}
	c.logWriters = map[string]*channelLogWriter{}
}

// Adds a change to a single channel-log (asynchronously)
func (c *changesWriter) addToChangeLog(channelName string, entry channels.LogEntry, parentRevID string) {
	c.logWriterForChannel(channelName).addChange(entry, parentRevID)
}

// Saves a channel log (asynchronously), _if_ there isn't already one in the database.
func (c *changesWriter) addChangeLog(channelName string, log *channels.ChangeLog) {
	c.logWriterForChannel(channelName).addChannelLog(log)
}

// Loads a channel's log from the database and returns it.
func (c *changesWriter) getChangeLog(channelName string, afterSeq uint64) (*channels.ChangeLog, error) {
	if raw, err := c.bucket.GetRaw(channelLogDocID(channelName)); err == nil {
		log, err := decodeChannelLog(raw)
		if err == nil {
			log.FilterAfter(afterSeq)
		}
		return log, err
	} else {
		if base.IsDocNotFoundError(err) {
			err = nil
		}
		return nil, err
	}
}

// Internal: returns a channelLogWriter that writes to the given channel.
func (c *changesWriter) logWriterForChannel(channelName string) *channelLogWriter {
	c.lock.Lock()
	defer c.lock.Unlock()
	logWriter := c.logWriters[channelName]
	if logWriter == nil {
		logWriter = newChannelLogWriter(c.bucket, channelName)
		c.logWriters[channelName] = logWriter
	}
	return logWriter
}

//////// CHANNEL LOG WRITER

// Writes changes to a single channel log.
type channelLogWriter struct {
	bucket      base.Bucket
	channelName string
	io          chan *changeEntry
	awake       chan bool
}

type changeEntry struct {
	logEntry       *channels.LogEntry
	parentRevID    string
	replacementLog *channels.ChangeLog
}

// Max number of pending writes
const kChannelLogWriterQueueLength = 1000

// Creates a channelLogWriter for a particular channel.
func newChannelLogWriter(bucket base.Bucket, channelName string) *channelLogWriter {
	c := &channelLogWriter{
		bucket:      bucket,
		channelName: channelName,
		io:          make(chan *changeEntry, kChannelLogWriterQueueLength),
		awake:       make(chan bool, 1),
	}
	go func() {
		// This is the goroutine the channelLogWriter runs:
		for {
			if changes := c.readChanges_(); changes != nil {
				c.addToChangeLog_(c.massageChanges(changes))
				time.Sleep(50) // lowering rate helps to coalesce changes, limiting # of writes
			} else {
				break // client called close
			}
		}
		close(c.awake)
	}()
	return c
}

// Queues a change to be written to the change-log.
func (c *channelLogWriter) addChange(entry channels.LogEntry, parentRevID string) {
	c.io <- &changeEntry{logEntry: &entry, parentRevID: parentRevID}
}

// Queues an entire new channel log to be written
func (c *channelLogWriter) addChannelLog(log *channels.ChangeLog) {
	c.io <- &changeEntry{replacementLog: log}
}

// Stops the background goroutine of a channelLogWriter.
func (c *channelLogWriter) stop() {
	close(c.io)
	<-c.awake // block until goroutine finishes
}

func (c *channelLogWriter) readChange_() *changeEntry {
	for {
		entry, ok := <-c.io
		if !ok {
			return nil
		} else if entry.replacementLog != nil {
			// Request to create the channel log if it doesn't exist:
			c.addChangeLog_(entry.replacementLog)
		} else {
			return entry
		}
	}
}

// Reads all available changes from io and returns them as an array, or nil if io is closed.
func (c *channelLogWriter) readChanges_() []*changeEntry {
	// Read first:
	entry := c.readChange_()
	if entry == nil {
		return nil
	}
	// Try to read more as long as they're available:
	entries := []*changeEntry{entry}
loop:
	for len(entries) < kChannelLogWriterQueueLength {
		var ok bool
		select {
		case entry, ok = <-c.io:
			if !ok {
				break loop
			} else if entry.replacementLog != nil {
				// Request to create the channel log if it doesn't exist:
				c.addChangeLog_(entry.replacementLog)
			} else {
				entries = append(entries, entry)
			}
		default:
			break loop
		}
	}
	return entries
}

// Simplifies an array of changes before they're appended to the channel log.
func (c *channelLogWriter) massageChanges(changes []*changeEntry) []*changeEntry {
	sort.Sort(changeEntryList(changes))
	return changes
}

// Saves a channel log, _if_ there isn't already one in the database.
func (c *channelLogWriter) addChangeLog_(log *channels.ChangeLog) (added bool, err error) {
	added, err = c.bucket.AddRaw(channelLogDocID(c.channelName), 0, encodeChannelLog(log))
	if added {
		base.LogTo("Changes", "Added missing channel-log %q with %d entries",
			c.channelName, log.Len())
	} else {
		base.LogTo("Changes", "Didn't add channel-log %q with %d entries (err=%v)",
			c.channelName, log.Len())
	}
	return
}

type changeEntryList []*changeEntry

func (cl changeEntryList) Len() int { return len(cl) }
func (cl changeEntryList) Less(i, j int) bool {
	return cl[i].logEntry.Sequence < cl[j].logEntry.Sequence
}
func (cl changeEntryList) Swap(i, j int) { cl[i], cl[j] = cl[j], cl[i] }

// Writes new changes to my channel log document.
func (c *channelLogWriter) addToChangeLog_(entries []*changeEntry) error {
	var fullUpdate bool
	var removedCount int
	fullUpdateAttempts := 0

	logDocID := channelLogDocID(c.channelName)
	err := c.bucket.WriteUpdate(logDocID, 0, func(currentValue []byte) ([]byte, walrus.WriteOptions, error) {
		// (Be careful: this block can be invoked multiple times if there are races!)
		// Should I do a full update of the change log, removing older entries to limit its size?
		// This has to be done occasionaly, but it's slower than simply appending to it. This
		// test is a heuristic that seems to strike a good balance in practice:
		fullUpdate = AlwaysCompactChangeLog ||
			(len(currentValue) > 20000 && (rand.Intn(100) < len(currentValue)/5000))
		removedCount = 0

		numToKeep := MaxChangeLogLength - len(entries)
		if len(currentValue) == 0 || numToKeep <= 0 {
			// If the log was empty, create a new log and return:
			if numToKeep < 0 {
				entries = entries[-numToKeep:]
			}
			channelLog := channels.ChangeLog{}
			for _, entry := range entries {
				channelLog.Add(*entry.logEntry)
			}
			return encodeChannelLog(&channelLog), walrus.Raw, nil
		} else if fullUpdate {
			fullUpdateAttempts++
			var newValue bytes.Buffer
			removedCount = channels.TruncateEncodedChangeLog(bytes.NewReader(currentValue),
				numToKeep, numToKeep/2, &newValue)
			if removedCount > 0 {
				for _, entry := range entries {
					entry.logEntry.Encode(&newValue, entry.parentRevID)
				}
				return newValue.Bytes(), walrus.Raw, nil
			}
		}

		// Append the encoded form of the new entries to the raw log bytes:
		w := bytes.NewBuffer(make([]byte, 0, 50000))
		for _, entry := range entries {
			entry.logEntry.Encode(w, entry.parentRevID)
		}
		currentValue = append(currentValue, w.Bytes()...)
		return currentValue, walrus.Raw, nil
	})

	base.LogTo("Changes", "Wrote %d sequence(s) to channel log %q", len(entries), c.channelName)

	/*if fullUpdate {
		base.Log("Removed %d entries from %q", removedCount, c.channelName)
	} else if fullUpdateAttempts > 0 {
		base.Log("Attempted to remove entries %d times but failed", fullUpdateAttempts)
	}*/
	return err
}

//////// SUBROUTINES:

// The "2" is a version tag. Update this if we change the format later.
const kChannelLogDocType = "log2"
const kChannelLogKeyPrefix = "_sync:" + kChannelLogDocType + ":"

func channelLogDocID(channelName string) string {
	return kChannelLogKeyPrefix + channelName
}

func decodeChannelLog(raw []byte) (*channels.ChangeLog, error) {
	if raw == nil {
		return nil, nil
	}
	return channels.DecodeChangeLog(bytes.NewReader(raw)), nil
}

func encodeChannelLog(log *channels.ChangeLog) []byte {
	if log == nil {
		return nil
	}
	raw := bytes.NewBuffer(make([]byte, 0, 50000))
	log.Encode(raw)
	return raw.Bytes()
}