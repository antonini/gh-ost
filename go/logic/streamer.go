/*
   Copyright 2016 GitHub Inc.
	 See https://github.com/github/gh-osc/blob/master/LICENSE
*/

package logic

import (
	gosql "database/sql"
	"fmt"
	"strings"

	"github.com/github/gh-osc/go/base"
	"github.com/github/gh-osc/go/binlog"
	"github.com/github/gh-osc/go/mysql"

	"github.com/outbrain/golib/log"
	"github.com/outbrain/golib/sqlutils"
)

type BinlogEventListener struct {
	async        bool
	databaseName string
	tableName    string
	onDmlEvent   func(event *binlog.BinlogDMLEvent) error
}

const (
	EventsChannelBufferSize = 1
)

// EventsStreamer reads data from binary logs and streams it on. It acts as a publisher,
// and interested parties may subscribe for per-table events.
type EventsStreamer struct {
	connectionConfig      *mysql.ConnectionConfig
	db                    *gosql.DB
	migrationContext      *base.MigrationContext
	nextBinlogCoordinates *mysql.BinlogCoordinates
	listeners             [](*BinlogEventListener)
	eventsChannel         chan *binlog.BinlogEntry
	binlogReader          binlog.BinlogReader
}

func NewEventsStreamer() *EventsStreamer {
	return &EventsStreamer{
		connectionConfig: base.GetMigrationContext().InspectorConnectionConfig,
		migrationContext: base.GetMigrationContext(),
		listeners:        [](*BinlogEventListener){},
		eventsChannel:    make(chan *binlog.BinlogEntry, EventsChannelBufferSize),
	}
}

func (this *EventsStreamer) AddListener(
	async bool, databaseName string, tableName string, onDmlEvent func(event *binlog.BinlogDMLEvent) error) (err error) {
	if databaseName == "" {
		return fmt.Errorf("Empty database name in AddListener")
	}
	if tableName == "" {
		return fmt.Errorf("Empty table name in AddListener")
	}
	listener := &BinlogEventListener{
		async:        async,
		databaseName: databaseName,
		tableName:    tableName,
		onDmlEvent:   onDmlEvent,
	}
	this.listeners = append(this.listeners, listener)
	return nil
}

func (this *EventsStreamer) notifyListeners(binlogEvent *binlog.BinlogDMLEvent) {
	for _, listener := range this.listeners {
		if strings.ToLower(listener.databaseName) != strings.ToLower(binlogEvent.DatabaseName) {
			continue
		}
		if strings.ToLower(listener.tableName) != strings.ToLower(binlogEvent.TableName) {
			continue
		}
		onDmlEvent := listener.onDmlEvent
		if listener.async {
			go func() {
				onDmlEvent(binlogEvent)
			}()
		} else {
			onDmlEvent(binlogEvent)
		}
	}
}

func (this *EventsStreamer) InitDBConnections() (err error) {
	EventsStreamerUri := this.connectionConfig.GetDBUri(this.migrationContext.DatabaseName)
	if this.db, _, err = sqlutils.GetDB(EventsStreamerUri); err != nil {
		return err
	}
	if err := this.validateConnection(); err != nil {
		return err
	}
	if err := this.readCurrentBinlogCoordinates(); err != nil {
		return err
	}
	goMySQLReader, err := binlog.NewGoMySQLReader(this.migrationContext.InspectorConnectionConfig)
	if err != nil {
		return err
	}
	if err := goMySQLReader.ConnectBinlogStreamer(*this.nextBinlogCoordinates); err != nil {
		return err
	}
	this.binlogReader = goMySQLReader

	return nil
}

// validateConnection issues a simple can-connect to MySQL
func (this *EventsStreamer) validateConnection() error {
	query := `select @@global.port`
	var port int
	if err := this.db.QueryRow(query).Scan(&port); err != nil {
		return err
	}
	if port != this.connectionConfig.Key.Port {
		return fmt.Errorf("Unexpected database port reported: %+v", port)
	}
	log.Infof("connection validated on %+v", this.connectionConfig.Key)
	return nil
}

// validateGrants verifies the user by which we're executing has necessary grants
// to do its thang.
func (this *EventsStreamer) readCurrentBinlogCoordinates() error {
	query := `show /* gh-osc readCurrentBinlogCoordinates */ master status`
	foundMasterStatus := false
	err := sqlutils.QueryRowsMap(this.db, query, func(m sqlutils.RowMap) error {
		this.nextBinlogCoordinates = &mysql.BinlogCoordinates{
			LogFile: m.GetString("File"),
			LogPos:  m.GetInt64("Position"),
		}
		foundMasterStatus = true

		return nil
	})
	if err != nil {
		return err
	}
	if !foundMasterStatus {
		return fmt.Errorf("Got no results from SHOW MASTER STATUS. Bailing out")
	}
	log.Debugf("Streamer binlog coordinates: %+v", *this.nextBinlogCoordinates)
	return nil
}

// StreamEvents will begin streaming events. It will be blocking, so should be
// executed by a goroutine
func (this *EventsStreamer) StreamEvents(canStopStreaming func() bool) error {
	go func() {
		for binlogEntry := range this.eventsChannel {
			if binlogEntry.DmlEvent != nil {
				this.notifyListeners(binlogEntry.DmlEvent)
			}
		}
	}()
	return this.binlogReader.StreamEvents(canStopStreaming, this.eventsChannel)
}