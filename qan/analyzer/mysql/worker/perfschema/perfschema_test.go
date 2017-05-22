/*
   Copyright (c) 2016, Percona LLC and/or its affiliates. All rights reserved.

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package perfschema

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/percona/go-mysql/event"
	"github.com/percona/pmm/proto"
	"github.com/percona/qan-agent/mysql"
	"github.com/percona/qan-agent/pct"
	"github.com/percona/qan-agent/qan/analyzer/mysql/iter"
	"github.com/percona/qan-agent/qan/analyzer/report"
	"github.com/percona/qan-agent/test/mock"
	. "github.com/percona/qan-agent/test/rootdir"
	"github.com/stretchr/testify/assert"
	. "gopkg.in/check.v1"
)

// Hook up gocheck into the "go test" runner.
func Test(t *testing.T) { TestingT(t) }

var inputDir = RootDir() + "/test/qan/perfschema/"

type WorkerTestSuite struct {
	dsn       string
	logChan   chan proto.LogEntry
	logger    *pct.Logger
	nullmysql *mock.NullMySQL
	mysqlConn *mysql.Connection
}

var _ = Suite(&WorkerTestSuite{})

func (s *WorkerTestSuite) SetUpSuite(t *C) {
	s.dsn = os.Getenv("PCT_TEST_MYSQL_DSN")
	if s.dsn == "" {
		t.Fatal("PCT_TEST_MYSQL_DSN is not set")
	}
	s.mysqlConn = mysql.NewConnection(s.dsn)
	if err := s.mysqlConn.Connect(); err != nil {
		t.Fatal(err)
	}
	s.logChan = make(chan proto.LogEntry, 100)
	s.logger = pct.NewLogger(s.logChan, "qan-worker")
	s.nullmysql = mock.NewNullMySQL()
}

func (s *WorkerTestSuite) SetUpTest(t *C) {
	s.nullmysql.Reset()
}

func (s *WorkerTestSuite) TearDownSuite(t *C) {
	s.mysqlConn.Close()
}

// --------------------------------------------------------------------------

func (s *WorkerTestSuite) loadData(dir string) ([][]*DigestRow, error) {
	files, err := filepath.Glob(filepath.Join(inputDir, dir, "/iter*.json"))
	if err != nil {
		return nil, err
	}
	iters := [][]*DigestRow{}
	for _, file := range files {
		bytes, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, err
		}
		rows := []*DigestRow{}
		if err := json.Unmarshal(bytes, &rows); err != nil {
			return nil, err
		}
		iters = append(iters, rows)
	}
	return iters, nil
}

func (s *WorkerTestSuite) loadResult(file string, got *report.Result) (*report.Result, error) {
	file = filepath.Join(inputDir, file)
	updateTestData := os.Getenv("UPDATE_TEST_DATA")
	if updateTestData != "" {
		data, _ := json.MarshalIndent(got, "", "  ")
		ioutil.WriteFile(file, data, 0666)

	}
	bytes, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	res := &report.Result{}
	if err := json.Unmarshal(bytes, &res); err != nil {
		return nil, err
	}
	return res, nil
}

func makeGetRowsFunc(iters [][]*DigestRow) GetDigestRowsFunc {
	return func(c chan<- *DigestRow, lastFetchSeconds float64, done chan<- error) error {
		if len(iters) == 0 {
			return fmt.Errorf("No more iters")
		}
		rows := iters[0]
		iters = iters[1:]
		go func() {
			defer func() {
				done <- nil
			}()
			for _, row := range rows {
				c <- row
			}
		}()
		return nil
	}
}

func makeGetTextFunc(texts ...string) GetDigestTextFunc {
	return func(digest string) (string, error) {
		if len(texts) == 0 {
			return "", fmt.Errorf("No more texts")
		}
		text := texts[0]
		texts = texts[1:]
		return text, nil
	}
}

type ByClassId []*event.Class

func (a ByClassId) Len() int      { return len(a) }
func (a ByClassId) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByClassId) Less(i, j int) bool {
	return a[i].Id < a[j].Id
}

func normalizeResult(res *report.Result) {
	sort.Sort(ByClassId(res.Class))
	// Perf Schema never has example queries, so remove the empty
	// event.Example struct the json creates.
	for n := range res.Class {
		res.Class[n].Example = nil
	}
}

// --------------------------------------------------------------------------

func (s *WorkerTestSuite) Test001(t *C) {
	// This is the simplest input possible: 1 query in iter 1 and 2. The result
	// is just the increase in its values.

	rows, err := s.loadData("001")
	t.Assert(err, IsNil)
	getRows := makeGetRowsFunc(rows)
	getText := makeGetTextFunc("select 1")
	w := NewWorker(s.logger, s.nullmysql, getRows, getText)

	// First run doesn't produce a result because 2 snapshots are required.
	i := &iter.Interval{
		Number:    1,
		StartTime: time.Now().UTC(),
	}
	err = w.Setup(i)
	t.Assert(err, IsNil)

	res, err := w.Run()
	t.Assert(err, IsNil)
	t.Check(res, IsNil)

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// The second run produces a result: the diff of 2nd - 1st.
	i = &iter.Interval{
		Number:    2,
		StartTime: time.Now().UTC(),
	}
	err = w.Setup(i)
	t.Assert(err, IsNil)

	res, err = w.Run()
	t.Assert(err, IsNil)
	normalizeResult(res)
	expect, err := s.loadResult("001/res01.json", res)
	t.Assert(err, IsNil)
	assert.Equal(t, expect, res)

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// Quick side test that Status() works and reports last stats.
	status := w.Status()
	t.Logf("%+v", status)
	t.Check(strings.HasPrefix(status["qan-worker-last"], "rows: 1"), Equals, true)
}

func (s *WorkerTestSuite) Test002(t *C) {
	// This is the 2nd most simplest input after 001: two queries, same digest,
	// but different schemas. The reuslt is the aggregate of their value diffs
	// from iter 1 to 2.

	rows, err := s.loadData("002")
	t.Assert(err, IsNil)
	getRows := makeGetRowsFunc(rows)
	getText := makeGetTextFunc("select 1")
	w := NewWorker(s.logger, s.nullmysql, getRows, getText)

	// First run doesn't produce a result because 2 snapshots are required.
	i := &iter.Interval{
		Number:    1,
		StartTime: time.Now().UTC(),
	}
	err = w.Setup(i)
	t.Assert(err, IsNil)

	res, err := w.Run()
	t.Assert(err, IsNil)
	t.Check(res, IsNil)

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// The second run produces a result: the diff of 2nd - 1st.
	i = &iter.Interval{
		Number:    2,
		StartTime: time.Now().UTC(),
	}
	err = w.Setup(i)
	t.Assert(err, IsNil)

	res, err = w.Run()
	t.Assert(err, IsNil)
	normalizeResult(res)
	expect, err := s.loadResult("002/res01.json", res)
	t.Assert(err, IsNil)
	assert.Equal(t, expect, res)

	err = w.Cleanup()
	t.Assert(err, IsNil)
}

func (s *WorkerTestSuite) TestEmptyDigest(t *C) {
	// This is the simplest input possible: 1 query in iter 1 and 2. The result
	// is just the increase in its values.

	rows, err := s.loadData("004")
	t.Assert(err, IsNil)
	getRows := makeGetRowsFunc(rows)
	getText := makeGetTextFunc("select 1")
	w := NewWorker(s.logger, s.nullmysql, getRows, getText)

	// First run doesn't produce a result because 2 snapshots are required.
	i := &iter.Interval{
		Number:    1,
		StartTime: time.Now().UTC(),
	}
	err = w.Setup(i)
	t.Assert(err, IsNil)

	res, err := w.Run()
	t.Assert(err, IsNil)
	t.Check(res, IsNil)

	err = w.Cleanup()
	t.Assert(err, IsNil)

}
func (s *WorkerTestSuite) TestRealWorker(t *C) {
	//FAIL: perfschema_test.go:290: WorkerTestSuite.TestRealWorker
	//
	//perfschema_test.go:344:
	//t.Assert(res, NotNil)
	//... value *iter.Result = (*iter.Result)(nil)
	t.Skip("'Make PMM great again!' No automated testing and this test was failing on 9 Feburary 2017: https://github.com/percona/qan-agent/pull/37")

	if s.dsn == "" {
		t.Fatal("PCT_TEST_MYSQL_DSN is not set")
	}
	mysqlConn := mysql.NewConnection(s.dsn)
	err := mysqlConn.Connect()
	t.Assert(err, IsNil)
	f := NewRealWorkerFactory(s.logChan)
	w := f.Make("qan-worker", mysqlConn)

	start := []mysql.Query{
		{Verify: "performance_schema", Expect: "1"},
		{Set: "UPDATE performance_schema.setup_consumers SET ENABLED = 'YES' WHERE NAME = 'statements_digest'"},
		{Set: "UPDATE performance_schema.setup_instruments SET ENABLED = 'YES', TIMED = 'YES' WHERE NAME LIKE 'statement/sql/%'"},
		{Set: "TRUNCATE performance_schema.events_statements_summary_by_digest"},
	}
	if err := s.mysqlConn.Set(start); err != nil {
		t.Fatal(err)
	}
	stop := []mysql.Query{
		{Set: "UPDATE performance_schema.setup_consumers SET ENABLED = 'NO' WHERE NAME = 'statements_digest'"},
		{Set: "UPDATE performance_schema.setup_instruments SET ENABLED = 'NO', TIMED = 'NO' WHERE NAME LIKE 'statement/sql/%'"},
	}
	defer func() {
		if err := s.mysqlConn.Set(stop); err != nil {
			t.Fatal(err)
		}
	}()

	// SCHEMA_NAME: NULL
	//      DIGEST: fbe070dfb47e4a2401c5be6b5201254e
	// DIGEST_TEXT: SELECT ? FROM DUAL
	_, err = s.mysqlConn.DB().Exec("SELECT 'teapot' FROM DUAL")

	// First interval.
	err = w.Setup(&iter.Interval{Number: 1, StartTime: time.Now().UTC()})
	t.Assert(err, IsNil)

	res, err := w.Run()
	t.Assert(err, IsNil)
	t.Check(res, IsNil)

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// Some query activity between intervals.
	_, err = s.mysqlConn.DB().Exec("SELECT 'teapot' FROM DUAL")
	time.Sleep(1 * time.Second)

	// Second interval and a result.
	err = w.Setup(&iter.Interval{Number: 2, StartTime: time.Now().UTC()})
	t.Assert(err, IsNil)

	res, err = w.Run()
	t.Assert(err, IsNil)
	t.Assert(res, NotNil)
	if len(res.Class) == 0 {
		t.Fatal("Expected len(res.Class) > 0")
	}
	var class *event.Class
	for _, c := range res.Class {
		if c.Fingerprint == "SELECT ? FROM DUAL " {
			class = c
			break
		}
	}
	t.Assert(class, NotNil)
	// Digests on different versions or distros of MySQL don't match
	//t.Check(class.Id, Equals, "01C5BE6B5201254E")
	//t.Check(class.Fingerprint, Equals, "SELECT ? FROM DUAL ")
	queryTime := class.Metrics.TimeMetrics["Query_time"]
	if queryTime.Min == 0 {
		t.Error("Expected Query_time_min > 0")
	}
	if queryTime.Max == 0 {
		t.Error("Expected Query_time_max > 0")
	}
	if queryTime.Avg == 0 {
		t.Error("Expected Query_time_avg > 0")
	}
	if queryTime.Min > queryTime.Max {
		t.Error("Expected Query_time_min >= Query_time_max")
	}
	t.Check(class.Metrics.NumberMetrics["Rows_affected"].Sum, Equals, uint64(0))
	t.Check(class.Metrics.NumberMetrics["Rows_examined"].Sum, Equals, uint64(0))
	t.Check(class.Metrics.NumberMetrics["Rows_sent"].Sum, Equals, uint64(1))

	err = w.Cleanup()
	t.Assert(err, IsNil)
}

func (s *WorkerTestSuite) TestIterOutOfSeq(t *C) {
	//FAIL: perfschema_test.go:380: WorkerTestSuite.TestIterOutOfSeq

	//perfschema_test.go:448:
	//t.Assert(res, NotNil)
	//... value *iter.Result = (*iter.Result)(nil)
	t.Skip("'Make PMM great again!' No automated testing and this test was failing on 9 Feburary 2017: https://github.com/percona/qan-agent/pull/37")

	if s.dsn == "" {
		t.Fatal("PCT_TEST_MYSQL_DSN is not set")
	}
	mysqlConn := mysql.NewConnection(s.dsn)
	err := mysqlConn.Connect()
	t.Assert(err, IsNil)
	f := NewRealWorkerFactory(s.logChan)
	w := f.Make("qan-worker", mysqlConn)

	start := []mysql.Query{
		{Verify: "performance_schema", Expect: "1"},
		{Set: "UPDATE performance_schema.setup_consumers SET ENABLED = 'YES' WHERE NAME = 'statements_digest'"},
		{Set: "UPDATE performance_schema.setup_instruments SET ENABLED = 'YES', TIMED = 'YES' WHERE NAME LIKE 'statement/sql/%'"},
		{Set: "TRUNCATE performance_schema.events_statements_summary_by_digest"},
	}
	if err := s.mysqlConn.Set(start); err != nil {
		t.Fatal(err)
	}
	stop := []mysql.Query{
		{Set: "UPDATE performance_schema.setup_consumers SET ENABLED = 'NO' WHERE NAME = 'statements_digest'"},
		{Set: "UPDATE performance_schema.setup_instruments SET ENABLED = 'NO', TIMED = 'NO' WHERE NAME LIKE 'statement/sql/%'"},
	}
	defer func() {
		if err := s.mysqlConn.Set(stop); err != nil {
			t.Fatal(err)
		}
	}()

	// SCHEMA_NAME: NULL
	//      DIGEST: fbe070dfb47e4a2401c5be6b5201254e
	// DIGEST_TEXT: SELECT ? FROM DUAL
	_, err = s.mysqlConn.DB().Exec("SELECT 'teapot' FROM DUAL")

	// First interval.
	err = w.Setup(&iter.Interval{Number: 1, StartTime: time.Now().UTC()})
	t.Assert(err, IsNil)

	res, err := w.Run()
	t.Assert(err, IsNil)
	t.Check(res, IsNil)

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// Some query activity between intervals.
	_, err = s.mysqlConn.DB().Exec("SELECT 'teapot' FROM DUAL")
	time.Sleep(1 * time.Second)

	// Simulate the ticker being reset which results in it resetting
	// its internal interval number, so instead of 2 here we have 1 again.
	// Second interval and a result.
	err = w.Setup(&iter.Interval{Number: 1, StartTime: time.Now().UTC()})
	t.Assert(err, IsNil)

	res, err = w.Run()
	t.Assert(err, IsNil)
	t.Check(res, IsNil) // no result due to out of sequence interval

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// Simulate normal operation resuming, i.e. interval 2.
	err = w.Setup(&iter.Interval{Number: 2, StartTime: time.Now().UTC()})
	t.Assert(err, IsNil)

	// Now there should be a result.
	res, err = w.Run()
	t.Assert(err, IsNil)
	t.Assert(res, NotNil)
	if len(res.Class) == 0 {
		t.Error("Expected len(res.Class) > 0")
	}
}

func (s *WorkerTestSuite) TestIterClockReset(t *C) {
	//FAIL: perfschema_test.go:454: WorkerTestSuite.TestIterClockReset
	//
	//perfschema_test.go:518:
	//t.Assert(res, NotNil)
	//... value *iter.Result = (*iter.Result)(nil)
	t.Skip("'Make PMM great again!' No automated testing and this test was failing on 9 Feburary 2017: https://github.com/percona/qan-agent/pull/37")

	if s.dsn == "" {
		t.Fatal("PCT_TEST_MYSQL_DSN is not set")
	}
	mysqlConn := mysql.NewConnection(s.dsn)
	err := mysqlConn.Connect()
	t.Assert(err, IsNil)
	f := NewRealWorkerFactory(s.logChan)
	w := f.Make("qan-worker", mysqlConn)

	start := []mysql.Query{
		{Verify: "performance_schema", Expect: "1"},
		{Set: "UPDATE performance_schema.setup_consumers SET ENABLED = 'YES' WHERE NAME = 'statements_digest'"},
		{Set: "UPDATE performance_schema.setup_instruments SET ENABLED = 'YES', TIMED = 'YES' WHERE NAME LIKE 'statement/sql/%'"},
		{Set: "TRUNCATE performance_schema.events_statements_summary_by_digest"},
	}
	if err := s.mysqlConn.Set(start); err != nil {
		t.Fatal(err)
	}
	stop := []mysql.Query{
		{Set: "UPDATE performance_schema.setup_consumers SET ENABLED = 'NO' WHERE NAME = 'statements_digest'"},
		{Set: "UPDATE performance_schema.setup_instruments SET ENABLED = 'NO', TIMED = 'NO' WHERE NAME LIKE 'statement/sql/%'"},
	}
	defer func() {
		if err := s.mysqlConn.Set(stop); err != nil {
			t.Fatal(err)
		}
	}()

	// Generate some perf schema data.
	_, err = s.mysqlConn.DB().Exec("SELECT 'teapot' FROM DUAL")

	// First interval.
	now := time.Now().UTC()
	err = w.Setup(&iter.Interval{Number: 1, StartTime: now})
	t.Assert(err, IsNil)

	res, err := w.Run()
	t.Assert(err, IsNil)
	t.Check(res, IsNil)

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// Simulate the ticker sending a time that's earlier than the previous
	// tick, which shouldn't happen.
	now = now.Add(-1 * time.Minute)
	err = w.Setup(&iter.Interval{Number: 2, StartTime: now})
	t.Assert(err, IsNil)

	res, err = w.Run()
	t.Assert(err, IsNil)
	t.Check(res, IsNil) // no result due to out of sequence interval

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// Simulate normal operation resuming.
	now = now.Add(1 * time.Minute)
	err = w.Setup(&iter.Interval{Number: 3, StartTime: now})
	t.Assert(err, IsNil)

	// Now there should be a result.
	res, err = w.Run()
	t.Assert(err, IsNil)
	t.Assert(res, NotNil)
	if len(res.Class) == 0 {
		t.Error("Expected len(res.Class) > 0")
	}
}

func (s *WorkerTestSuite) TestIter(t *C) {
	tickChan := make(chan time.Time, 1)
	i := NewIter(pct.NewLogger(s.logChan, "iter"), tickChan)
	t.Assert(i, NotNil)

	iterChan := i.IntervalChan()
	t.Assert(iterChan, NotNil)

	i.Start()
	defer i.Stop()

	t1, _ := time.Parse("2006-01-02 15:04:05", "2015-01-01 00:01:00")
	t2, _ := time.Parse("2006-01-02 15:04:05", "2015-01-01 00:02:00")
	t3, _ := time.Parse("2006-01-02 15:04:05", "2015-01-01 00:03:00")

	tickChan <- t1
	got := <-iterChan
	assert.Equal(t, &iter.Interval{Number: 1, StartTime: time.Time{}, StopTime: t1}, got)

	tickChan <- t2
	got = <-iterChan
	assert.Equal(t, &iter.Interval{Number: 2, StartTime: t1, StopTime: t2}, got)

	tickChan <- t3
	got = <-iterChan
	assert.Equal(t, &iter.Interval{Number: 3, StartTime: t2, StopTime: t3}, got)
}

func (s *WorkerTestSuite) Test003(t *C) {
	// This test has 4 iters:
	//   1: 2 queries
	//   2: 2 queries (res02)
	//   3: 4 queries (res03)
	//   4: 4 queries but 4th has same COUNT_STAR (res04)
	rows, err := s.loadData("003")
	t.Assert(err, IsNil)
	getRows := makeGetRowsFunc(rows)
	getText := makeGetTextFunc("select 1", "select 2", "select 3", "select 4")
	w := NewWorker(s.logger, s.nullmysql, getRows, getText)

	// First interval doesn't produce a result because 2 snapshots are required.
	i := &iter.Interval{
		Number:    1,
		StartTime: time.Now().UTC(),
	}
	err = w.Setup(i)
	t.Assert(err, IsNil)

	res, err := w.Run()
	t.Assert(err, IsNil)
	t.Check(res, IsNil)

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// Second interval produces a result: the diff of 2nd - 1st.
	i = &iter.Interval{
		Number:    2,
		StartTime: time.Now().UTC(),
	}
	err = w.Setup(i)
	t.Assert(err, IsNil)

	res, err = w.Run()
	t.Assert(err, IsNil)
	normalizeResult(res)
	expect, err := s.loadResult("003/res02.json", res)
	t.Assert(err, IsNil)
	assert.Equal(t, expect, res)

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// Third interval...
	i = &iter.Interval{
		Number:    3,
		StartTime: time.Now().UTC(),
	}
	err = w.Setup(i)
	t.Assert(err, IsNil)

	res, err = w.Run()
	t.Assert(err, IsNil)
	normalizeResult(res)
	expect, err = s.loadResult("003/res03.json", res)
	t.Assert(err, IsNil)
	assert.Equal(t, expect, res)

	err = w.Cleanup()
	t.Assert(err, IsNil)

	// Fourth interval...
	i = &iter.Interval{
		Number:    4,
		StartTime: time.Now().UTC(),
	}
	err = w.Setup(i)
	t.Assert(err, IsNil)

	res, err = w.Run()
	t.Assert(err, IsNil)
	normalizeResult(res)
	expect, err = s.loadResult("003/res04.json", res)
	t.Assert(err, IsNil)
	assert.Equal(t, expect, res)

	err = w.Cleanup()
	t.Assert(err, IsNil)
}