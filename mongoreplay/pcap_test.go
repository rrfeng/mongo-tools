package mongoreplay

import (
	"io"
	"os"
	"testing"

	mgo "github.com/10gen/llmgo"
	"github.com/10gen/llmgo/bson"
)

type verifyFunc func(*testing.T, *mgo.Session, *BufferedStatRecorder, *preprocessCursorManager)

func TestOpCommandFromPcapFileLiveDB(t *testing.T) {
	if err := teardownDB(); err != nil {
		t.Error(err)
	}
	if isMongosTestServer {
		t.Skipf("Skipping OpCommand test when running against mongos")
	}

	pcapFname := "op_command_2inserts.pcap"

	var verifier = func(t *testing.T, session *mgo.Session, statRecorder *BufferedStatRecorder, cursorMap *preprocessCursorManager) {
		t.Log("Verifying that the correct number of getmores were seen")
		coll := session.DB(testDB).C(testCollection)
		iter := coll.Find(bson.D{}).Sort("op_command_test").Iter()
		result := struct {
			TestNum int `bson:"op_command_test"`
		}{}

		t.Log("Querying database to ensure insert occured successfully")
		ind := 1
		for iter.Next(&result) {
			if result.TestNum != ind {
				t.Errorf("document number not matched. Expected: %v -- found %v", ind, result.TestNum)
			}
			ind++
		}
		if ind != 3 {
			t.Errorf("did not find the correct number of documents. Expected %v -- found %v", ind-1, 2)

		}
	}

	pcapTestHelper(t, pcapFname, false, verifier)
	if err := teardownDB(); err != nil {
		t.Error(err)
	}
}

func TestWireCompression(t *testing.T) {
	pcapFname := "compressed.pcap"
	var verifier = func(t *testing.T, session *mgo.Session, statRecorder *BufferedStatRecorder, cursorMap *preprocessCursorManager) {
		opsSeen := len(statRecorder.Buffer)
		if opsSeen != 24 {
			t.Errorf("Didn't seen the correct number of ops, expected 24 but saw %v", opsSeen)
		}
		coll := session.DB(testDB).C(testCollection)
		num, _ := coll.Count()

		if num != 1 {
			t.Error("Didn't find the expected single documents in the database")
		}

	}
	pcapTestHelper(t, pcapFname, true, verifier)
}

func TestSingleChannelGetMoreLiveDB(t *testing.T) {
	pcapFname := "getmore_single_channel.pcap"
	var verifier = func(t *testing.T, session *mgo.Session, statRecorder *BufferedStatRecorder, cursorMap *preprocessCursorManager) {
		getMoresSeen := 0
		for _, val := range statRecorder.Buffer {
			if val.OpType == "getmore" {
				getMoresSeen++
				if val.NumReturned > 0 {
					t.Errorf("Getmore shouldn't have returned anything, but returned %v", val.NumReturned)
				}
			}
		}
		if getMoresSeen != 8 {
			t.Errorf("Didn't seen the correct number of getmores, expected 8 but saw %v", getMoresSeen)
		}
		coll := session.DB(testDB).C(testCollection)
		num, _ := coll.Count()

		if num < 1 {
			t.Error("Didn't find any documents in the database")
		}

	}
	pcapTestHelper(t, pcapFname, true, verifier)
}

func TestMultiChannelGetMoreLiveDB(t *testing.T) {

	pcapFname := "getmore_multi_channel.pcap"
	var verifier = func(t *testing.T, session *mgo.Session, statRecorder *BufferedStatRecorder, cursorMap *preprocessCursorManager) {
		aggregationsSeen := 0
		getMoresSeen := 0
		for _, val := range statRecorder.Buffer {
			if val.OpType == "command" && val.Command == "aggregate" {
				aggregationsSeen++
			} else if val.OpType == "getmore" {
				getMoresSeen++
				if aggregationsSeen != 2 {
					t.Error("Getmore seen before cursor producing operation")
				}
				if val.NumReturned < 100 {
					t.Errorf("Getmore should have returned a full batch, but only returned %v", val.NumReturned)
				}
			}
		}
		if getMoresSeen != 8 {
			t.Errorf("Didn't seen the correct number of getmores, expected 8 but saw %v", getMoresSeen)
		}
		coll := session.DB(testDB).C(testCollection)
		num, _ := coll.Count()

		if num < 1 {
			t.Error("Didn't find any documents in the database")
		}

	}
	pcapTestHelper(t, pcapFname, true, verifier)
}

func pcapTestHelper(t *testing.T, pcapFname string, preprocess bool, verifier verifyFunc) {

	pcapFile := "testPcap/" + pcapFname
	if _, err := os.Stat(pcapFile); err != nil {
		t.Skipf("pcap file %v not present, skipping test", pcapFile)
	}

	if err := teardownDB(); err != nil {
		t.Error(err)
	}

	playbackFname := "pcap_test_run.tape"
	streamSettings := OpStreamSettings{
		PcapFile:      pcapFile,
		PacketBufSize: 9000,
	}
	t.Log("Opening op stream")
	ctx, err := getOpstream(streamSettings)
	if err != nil {
		t.Errorf("error opening opstream: %v\n", err)
	}

	playbackWriter, err := NewPlaybackWriter(playbackFname, false)
	defer os.Remove(playbackFname)
	if err != nil {
		t.Errorf("error opening playback file to write: %v\n", err)
	}

	t.Log("Recording playbackfile from pcap file")
	err = Record(ctx, playbackWriter, false)
	if err != nil {
		t.Errorf("error makign tape file: %v\n", err)
	}

	playbackReader, err := NewPlaybackFileReader(playbackFname, false)
	if err != nil {
		t.Errorf("error opening playback file to write: %v\n", err)
	}

	statCollector, _ := newStatCollector(testCollectorOpts, true, true)
	statRec := statCollector.StatRecorder.(*BufferedStatRecorder)
	context := NewExecutionContext(statCollector)

	var preprocessMap preprocessCursorManager
	if preprocess {
		opChan, errChan := NewOpChanFromFile(playbackReader, 1)
		preprocessMap, err := newPreprocessCursorManager(opChan)

		if err != nil {
			t.Errorf("error creating preprocess map: %v", err)
		}

		err = <-errChan
		if err != io.EOF {
			t.Errorf("error creating preprocess map: %v", err)
		}

		_, err = playbackReader.Seek(0, 0)
		if err != nil {
			t.Errorf("error seeking playbackfile: %v", err)
		}
		context.CursorIDMap = preprocessMap
	}

	opChan, errChan := NewOpChanFromFile(playbackReader, 1)

	t.Log("Reading ops from playback file")
	err = Play(context, opChan, testSpeed, currentTestURL, 1, 30)
	if err != nil {
		t.Errorf("error playing back recorded file: %v\n", err)
	}
	err = <-errChan
	if err != io.EOF {
		t.Errorf("error reading ops from file: %v\n", err)
	}
	//prepare a query for the database
	session, err := mgo.Dial(currentTestURL)
	if err != nil {
		t.Errorf("Error connecting to test server: %v", err)
	}
	verifier(t, session, statRec, &preprocessMap)

	if err := teardownDB(); err != nil {
		t.Error(err)
	}
}
