/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package binlog

import (
	crand "crypto/rand"
	"fmt"
	"math"
	"math/big"
	"sync"

	"golang.org/x/net/context"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/vt/dbconfigs"
	"vitess.io/vitess/go/vt/log"
)

var (
	// ErrBinlogUnavailable is returned by this library when we
	// cannot find a suitable binlog to satisfy the request.
	ErrBinlogUnavailable = fmt.Errorf("cannot find relevant binlogs on this server")
)

// SlaveConnection represents a connection to mysqld that pretends to be a slave
// connecting for replication. Each such connection must identify itself to
// mysqld with a server ID that is unique both among other SlaveConnections and
// among actual slaves in the topology.
type SlaveConnection struct {
	*mysql.Conn
	cp      dbconfigs.Connector
	slaveID uint32
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// We use a random slaveid deprecating IDPool which was causing problems with RDS
// either because it was using ids in the same range that we generate or because
// the pool reuses ids.
func getSlaveID() uint32 {
	fmt.Printf("In getSlaveId\n")
	max := big.NewInt(math.MaxInt32)
	id, err := crand.Int(crand.Reader, max)
	if err != nil {
		fmt.Printf("Error in getting slaveid %v\n", err)

		panic(fmt.Sprintf("Could not allocate slave id"))
	}
	fmt.Printf("getSlaveId returning %d\n", uint32(id.Int64()))
	return uint32(id.Int64())
}

// NewSlaveConnection creates a new slave connection to the mysqld instance.
func NewSlaveConnection(cp dbconfigs.Connector) (*SlaveConnection, error) {
	conn, err := connectForReplication(cp)
	if err != nil {
		return nil, err
	}

	sc := &SlaveConnection{
		Conn:    conn,
		cp:      cp,
		slaveID: getSlaveID(),
	}
	log.Infof("new slave connection: slaveID=%d", sc.slaveID)
	return sc, nil
}

// connectForReplication create a MySQL connection ready to use for replication.
func connectForReplication(cp dbconfigs.Connector) (*mysql.Conn, error) {
	ctx := context.Background()
	conn, err := cp.Connect(ctx)
	if err != nil {
		return nil, err
	}
	// Tell the server that we understand the format of events
	// that will be used if binlog_checksum is enabled on the server.
	if _, err := conn.ExecuteFetch("SET @master_binlog_checksum=@@global.binlog_checksum", 0, false); err != nil {
		return nil, fmt.Errorf("failed to set @master_binlog_checksum=@@global.binlog_checksum: %v", err)
	}

	return conn, nil
}

// StartBinlogDumpFromCurrent requests a replication binlog dump from
// the current position.
func (sc *SlaveConnection) StartBinlogDumpFromCurrent(ctx context.Context) (mysql.Position, <-chan mysql.BinlogEvent, error) {
	ctx, sc.cancel = context.WithCancel(ctx)

	masterPosition, err := sc.Conn.MasterPosition()
	if err != nil {
		return mysql.Position{}, nil, fmt.Errorf("failed to get master position: %v", err)
	}

	c, err := sc.StartBinlogDumpFromPosition(ctx, masterPosition)
	return masterPosition, c, err
}

// StartBinlogDumpFromPosition requests a replication binlog dump from
// the master mysqld at the given Position and then sends binlog
// events to the provided channel.
// The stream will continue in the background, waiting for new events if
// necessary, until the connection is closed, either by the master or
// by canceling the context.
//
// Note the context is valid and used until eventChan is closed.
func (sc *SlaveConnection) StartBinlogDumpFromPosition(ctx context.Context, startPos mysql.Position) (<-chan mysql.BinlogEvent, error) {
	ctx, sc.cancel = context.WithCancel(ctx)

	log.Infof("sending binlog dump command: startPos=%v, slaveID=%v", startPos, sc.slaveID)
	if err := sc.SendBinlogDumpCommand(sc.slaveID, startPos); err != nil {
		log.Errorf("couldn't send binlog dump command: %v", err)
		return nil, err
	}

	return sc.streamEvents(ctx), nil
}

// streamEvents returns a channel on which events are streamed.
func (sc *SlaveConnection) streamEvents(ctx context.Context) chan mysql.BinlogEvent {
	// FIXME(alainjobart) I think we can use a buffered channel for better performance.
	eventChan := make(chan mysql.BinlogEvent)

	// Start reading events.
	sc.wg.Add(1)
	go func() {
		defer func() {
			close(eventChan)
			sc.wg.Done()
		}()
		for {
			event, err := sc.Conn.ReadBinlogEvent()
			if err != nil {
				if sqlErr, ok := err.(*mysql.SQLError); ok && sqlErr.Number() == mysql.CRServerLost {
					// CRServerLost = Lost connection to MySQL server during query
					// This is not necessarily an error. It could just be that we closed
					// the connection from outside.
					log.Infof("connection closed during binlog stream (possibly intentional): %v", err)
					return
				}
				log.Errorf("read error while streaming binlog events: %v", err)
				return
			}

			select {
			case eventChan <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return eventChan
}

// StartBinlogDumpFromBinlogBeforeTimestamp requests a replication
// binlog dump from the master mysqld starting with a file that has
// timestamps smaller than the provided timestamp, and then sends
// binlog events to the provided channel.
//
// The startup phase will list all the binary logs, and find the one
// that has events starting strictly before the provided timestamp. It
// will then start from there, and stream all events. It is the
// responsibility of the calling site to filter the events more.
//
// MySQL 5.6+ note: we need to do it that way because of the way the
// GTIDSet works. In the previous two streaming functions, we pass in
// the full GTIDSet (that has the list of all transactions seen in
// the replication stream). In this case, we don't know it, all we
// have is the binlog file names. We depend on parsing the first
// PREVIOUS_GTIDS_EVENT event in the logs to get it. So we need the
// caller to parse that event, and it can't be skipped because its
// timestamp is lower. Then, for each subsequent event, the caller
// also needs to add the event GTID to its GTIDSet. Otherwise it won't
// be correct ever. So the caller really needs to build up its GTIDSet
// along the entire file, not just for events whose timestamp is in a
// given range.
//
// The stream will continue in the background, waiting for new events if
// necessary, until the connection is closed, either by the master or
// by canceling the context.
//
// Note the context is valid and used until eventChan is closed.
func (sc *SlaveConnection) StartBinlogDumpFromBinlogBeforeTimestamp(ctx context.Context, timestamp int64) (<-chan mysql.BinlogEvent, error) {
	ctx, sc.cancel = context.WithCancel(ctx)

	filename, err := sc.findFileBeforeTimestamp(ctx, timestamp)
	if err != nil {
		return nil, err
	}

	// Start dumping the logs. The position is '4' to skip the
	// Binlog File Header. See this page for more info:
	// https://dev.mysql.com/doc/internals/en/binlog-file.html
	if err := sc.Conn.WriteComBinlogDump(sc.slaveID, filename, 4, 0); err != nil {
		return nil, fmt.Errorf("failed to send the ComBinlogDump command: %v", err)
	}
	return sc.streamEvents(ctx), nil
}

func (sc *SlaveConnection) findFileBeforeTimestamp(ctx context.Context, timestamp int64) (filename string, err error) {
	// List the binlogs.
	binlogs, err := sc.Conn.ExecuteFetch("SHOW BINARY LOGS", 1000, false)
	if err != nil {
		return "", fmt.Errorf("failed to SHOW BINARY LOGS: %v", err)
	}

	// Start with the most recent binlog file until we find the right event.
	for binlogIndex := len(binlogs.Rows) - 1; binlogIndex >= 0; binlogIndex-- {
		// Exit the loop early if context is canceled.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		filename := binlogs.Rows[binlogIndex][0].ToString()
		blTimestamp, err := sc.getBinlogTimeStamp(filename)
		if err != nil {
			return "", err
		}
		if blTimestamp < timestamp {
			// The binlog timestamp is older: we've found a good starting point.
			return filename, nil
		}
	}

	log.Errorf("couldn't find an old enough binlog to match timestamp >= %v (looked at %v files)", timestamp, len(binlogs.Rows))
	return "", ErrBinlogUnavailable
}

func (sc *SlaveConnection) getBinlogTimeStamp(filename string) (blTimestamp int64, err error) {
	conn, err := connectForReplication(sc.cp)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if err := conn.WriteComBinlogDump(sc.slaveID, filename, 4, 0); err != nil {
		return 0, fmt.Errorf("failed to send the ComBinlogDump command: %v", err)
	}

	// Get the first event to get its timestamp. We skip
	// events that don't have timestamps (although it seems
	// most do anyway).
	for {
		event, err := conn.ReadBinlogEvent()
		if err != nil {
			return 0, fmt.Errorf("error reading binlog event %v: %v", filename, err)
		}
		if !event.IsValid() {
			return 0, fmt.Errorf("first event from binlog %v is not valid", filename)
		}
		if ts := event.Timestamp(); ts > 0 {
			return int64(ts), nil
		}
	}
}

// Close closes the slave connection, which also signals an ongoing dump
// started with StartBinlogDump() to stop and close its BinlogEvent channel.
// The ID for the slave connection is recycled back into the pool.
func (sc *SlaveConnection) Close() {
	if sc.Conn != nil {
		log.Infof("closing slave socket to unblock reads")
		sc.Conn.Close()

		// sc.cancel is set at the beginning of the StartBinlogDump*
		// methods. If we error out before then, it's nil.
		// Note we also may error out before adding 1 to sc.wg,
		// but then the Wait() still works.
		if sc.cancel != nil {
			log.Infof("waiting for slave dump thread to end")
			sc.cancel()
			sc.wg.Wait()
			sc.cancel = nil
		}

		log.Infof("closing slave MySQL client with slaveID %v", sc.slaveID)
		sc.Conn = nil
	}
}
