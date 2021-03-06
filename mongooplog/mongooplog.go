// Package mongooplog polls operations from the replication oplog of one server, and applies them to another.
package mongooplog

import (
	"fmt"
	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/log"
	"github.com/mongodb/mongo-tools/common/options"
	"github.com/mongodb/mongo-tools/common/util"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"time"
)

// MongoOplog is a container for the user-specified options for running mongooplog.
type MongoOplog struct {
	// standard tool options
	ToolOptions *options.ToolOptions

	// mongooplog-specific options
	SourceOptions *SourceOptions

	// session provider for the source server
	SessionProviderFrom *db.SessionProvider

	// session provider for the destination server
	SessionProviderTo *db.SessionProvider
}

// Run executes the mongooplog program.
func (mo *MongoOplog) Run() error {

	// split up the oplog namespace we are using
	oplogDB, oplogColl, err :=
		util.SplitAndValidateNamespace(mo.SourceOptions.OplogNS)

	if err != nil {
		return err
	}

	// the full oplog namespace needs to be specified
	if oplogColl == "" {
		return fmt.Errorf("the oplog namespace must specify a collection")
	}

	log.Logvf(log.DebugLow, "using oplog namespace `%v.%v`", oplogDB, oplogColl)

	// connect to the destination server
	toSession, err := mo.SessionProviderTo.GetSession()
	if err != nil {
		return fmt.Errorf("error connecting to destination db: %v", err)
	}
	defer toSession.Close()
	toSession.SetSocketTimeout(0)

	// purely for logging
	destServerStr := mo.ToolOptions.Host
	if mo.ToolOptions.Port != "" {
		destServerStr = destServerStr + ":" + mo.ToolOptions.Port
	}
	log.Logvf(log.DebugLow, "successfully connected to destination server `%v`", destServerStr)

	// connect to the source server
	fromSession, err := mo.SessionProviderFrom.GetSession()
	if err != nil {
		return fmt.Errorf("error connecting to source db: %v", err)
	}
	defer fromSession.Close()
	fromSession.SetSocketTimeout(0)

	log.Logvf(log.DebugLow, "successfully connected to source server `%v`", mo.SourceOptions.From)

	// set slave ok
	fromSession.SetMode(mgo.Eventual, true)

	// get the tailing cursor for the source server's oplog
	tail := buildTailingCursor(fromSession.DB(oplogDB).C(oplogColl),
		mo.SourceOptions)
	defer tail.Close()

	// read the cursor dry, applying ops to the destination
	// server in the process
	oplogEntry := &db.Oplog{}
	res := &db.ApplyOpsResponse{}

	log.Logv(log.DebugLow, "applying oplog entries...")

	oplogChan := make(chan db.Oplog)
	timer := time.NewTicker(5 * time.Second)

	opCount := 0
	go func() {
		for tail.Next(oplogEntry) {

			// skip noops
			if oplogEntry.Operation == "n" {
				log.Logvf(log.DebugHigh, "skipping no-op for namespace `%v`", oplogEntry.Namespace)
				continue
			}

			oplogChan <- *oplogEntry
			opCount++

			// print the first oplog to confirm with the target's latest oplog.
			if opCount == 1 {
				log.Logvf(log.Always, "Got first oplog with Timestamp: %v", oplogEntry.Timestamp>>32)
				log.Logvf(log.Always, "If this newer than target's last oplog, stop this.")
			}
		}

		// make sure there was no tailing error
		if err := tail.Err(); err != nil {
			log.Logvf(log.Always, "error querying oplog: %v", err)
			return
		}

		log.Logvf(log.DebugLow, "done applying %v oplog entries", opCount)
		return
	}()

	opsToApply := []db.Oplog{}
	maxSize := 10000
	for {
		select {
		case <-timer.C:
			if len(opsToApply) == 0 {
				continue
			}

			// apply the operation
			err := toSession.Run(bson.M{"applyOps": opsToApply}, res)

			if err != nil {
				return fmt.Errorf("error applying ops: %v", err)
			}

			// check the server's response for an issue
			if !res.Ok {
				return fmt.Errorf("server gave error applying ops: %v", res.ErrMsg)
			}

			log.Logvf(log.Always, "%v oplogs have been applied, total: %v. Last: %v", len(opsToApply), opCount, opsToApply[len(opsToApply)-1].Timestamp>>32)

			// reset the opsToApply silce
			opsToApply = opsToApply[:0]

		case opEntry := <-oplogChan:
			// prepare the op to be applied
			opsToApply = append(opsToApply, opEntry)

			// if there are too many oplogs, send.
			if len(opsToApply) >= maxSize {
				// apply the operation
				err := toSession.Run(bson.M{"applyOps": opsToApply}, res)

				if err != nil {
					return fmt.Errorf("error applying ops: %v", err)
				}

				// check the server's response for an issue
				if !res.Ok {
					return fmt.Errorf("server gave error applying ops: %v", res.ErrMsg)
				}

				log.Logvf(log.Always, "%v oplogs have been applied, total: %v. Last: %v", len(opsToApply), opCount, opEntry.Timestamp>>32)

				// reset the opsToApply silce
				opsToApply = opsToApply[:0]
			}
		}
	}
}

// get the cursor for the oplog collection, based on the options
// passed in to mongooplog
func buildTailingCursor(oplog *mgo.Collection,
	sourceOptions *SourceOptions) *mgo.Iter {

	// how many seconds in the past we need
	secondsInPast := time.Duration(sourceOptions.Seconds) * time.Second
	// the time threshold for oplog queries
	threshold := time.Now().Add(-secondsInPast)
	// convert to a unix timestamp (seconds since epoch)
	thresholdAsUnix := threshold.Unix()

	// shift it appropriately, to prepare it to be converted to an
	// oplog timestamp
	thresholdShifted := uint64(thresholdAsUnix) << 32

	// build the oplog query
	oplogQuery := bson.M{
		"ts": bson.M{
			"$gte": bson.MongoTimestamp(thresholdShifted),
		},
	}

	// wait up to 10min for an new oplog
	return oplog.Find(oplogQuery).LogReplay().Tail(600 * time.Second)
}
