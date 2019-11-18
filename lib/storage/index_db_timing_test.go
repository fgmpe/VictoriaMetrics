package storage

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/workingsetcache"
)

func BenchmarkRegexpFilterMatch(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		re := regexp.MustCompile(`.*foo-bar-baz.*`)
		b := []byte("fdsffd foo-bar-baz assd fdsfad dasf dsa")
		for pb.Next() {
			if !re.Match(b) {
				panic("BUG: regexp must match!")
			}
			b[0]++
		}
	})
}

func BenchmarkRegexpFilterMismatch(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		re := regexp.MustCompile(`.*foo-bar-baz.*`)
		b := []byte("fdsffd foo-bar sfddsf assd nmn,mfdsdsakj")
		for pb.Next() {
			if re.Match(b) {
				panic("BUG: regexp mustn't match!")
			}
			b[0]++
		}
	})
}

func BenchmarkIndexDBAddTSIDs(b *testing.B) {
	const recordsPerLoop = 1e3

	metricIDCache := workingsetcache.New(1234, time.Hour)
	metricNameCache := workingsetcache.New(1234, time.Hour)
	defer metricIDCache.Stop()
	defer metricNameCache.Stop()

	var hmCurr atomic.Value
	hmCurr.Store(&hourMetricIDs{})
	var hmPrev atomic.Value
	hmPrev.Store(&hourMetricIDs{})

	const dbName = "bench-index-db-add-tsids"
	db, err := openIndexDB(dbName, metricIDCache, metricNameCache, &hmCurr, &hmPrev)
	if err != nil {
		b.Fatalf("cannot open indexDB: %s", err)
	}
	defer func() {
		db.MustClose()
		if err := os.RemoveAll(dbName); err != nil {
			b.Fatalf("cannot remove indexDB: %s", err)
		}
	}()

	var goroutineID uint32

	b.ReportAllocs()
	b.SetBytes(recordsPerLoop)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var mn MetricName
		var tsid TSID
		mn.AccountID = atomic.AddUint32(&goroutineID, 1)

		// The most common tags.
		mn.Tags = []Tag{
			{
				Key: []byte("job"),
			},
			{
				Key: []byte("instance"),
			},
		}

		startOffset := 0
		for pb.Next() {
			benchmarkIndexDBAddTSIDs(db, &tsid, &mn, startOffset, recordsPerLoop)
			startOffset += recordsPerLoop
		}
	})
	b.StopTimer()
}

func benchmarkIndexDBAddTSIDs(db *indexDB, tsid *TSID, mn *MetricName, startOffset, recordsPerLoop int) {
	var metricName []byte
	is := db.getIndexSearch()
	defer db.putIndexSearch(is)
	for i := 0; i < recordsPerLoop; i++ {
		mn.MetricGroup = strconv.AppendUint(mn.MetricGroup[:0], uint64(i+startOffset), 10)
		for j := range mn.Tags {
			mn.Tags[j].Value = strconv.AppendUint(mn.Tags[j].Value[:0], uint64(i*j), 16)
		}
		mn.sortTags()
		metricName = mn.Marshal(metricName[:0])
		if err := is.GetOrCreateTSIDByName(tsid, metricName); err != nil {
			panic(fmt.Errorf("cannot insert record: %s", err))
		}
	}
}

func BenchmarkHeadPostingForMatchers(b *testing.B) {
	// This benchmark is equivalent to https://github.com/prometheus/prometheus/blob/23c0299d85bfeb5d9b59e994861553a25ca578e5/tsdb/head_bench_test.go#L52
	// See https://www.robustperception.io/evaluating-performance-and-correctness for more details.
	metricIDCache := workingsetcache.New(1234, time.Hour)
	metricNameCache := workingsetcache.New(1234, time.Hour)
	defer metricIDCache.Stop()
	defer metricNameCache.Stop()

	var hmCurr atomic.Value
	hmCurr.Store(&hourMetricIDs{})
	var hmPrev atomic.Value
	hmPrev.Store(&hourMetricIDs{})

	const dbName = "bench-head-posting-for-matchers"
	db, err := openIndexDB(dbName, metricIDCache, metricNameCache, &hmCurr, &hmPrev)
	if err != nil {
		b.Fatalf("cannot open indexDB: %s", err)
	}
	defer func() {
		db.MustClose()
		if err := os.RemoveAll(dbName); err != nil {
			b.Fatalf("cannot remove indexDB: %s", err)
		}
	}()

	// Fill the db with data as in https://github.com/prometheus/prometheus/blob/23c0299d85bfeb5d9b59e994861553a25ca578e5/tsdb/head_bench_test.go#L66
	const accountID = 34327843
	const projectID = 893433
	var mn MetricName
	var metricName []byte
	var tsid TSID
	addSeries := func(kvs ...string) {
		mn.Reset()
		for i := 0; i < len(kvs); i += 2 {
			mn.AddTag(kvs[i], kvs[i+1])
		}
		mn.sortTags()
		mn.AccountID = accountID
		mn.ProjectID = projectID
		metricName = mn.Marshal(metricName[:0])
		if err := db.createTSIDByName(&tsid, metricName); err != nil {
			b.Fatalf("cannot insert record: %s", err)
		}
	}
	for n := 0; n < 10; n++ {
		for i := 0; i < 100000; i++ {
			addSeries("i", strconv.Itoa(i), "n", strconv.Itoa(n), "j", "foo")
			// Have some series that won't be matched, to properly test inverted matches.
			addSeries("i", strconv.Itoa(i), "n", strconv.Itoa(n), "j", "bar")
			addSeries("i", strconv.Itoa(i), "n", "0_"+strconv.Itoa(n), "j", "bar")
			addSeries("i", strconv.Itoa(i), "n", "1_"+strconv.Itoa(n), "j", "bar")
			addSeries("i", strconv.Itoa(i), "n", "2_"+strconv.Itoa(n), "j", "foo")
		}
	}

	// Make sure all the items can be searched.
	db.tb.DebugFlush()
	b.ResetTimer()

	benchSearch := func(b *testing.B, tfs *TagFilters) {
		is := db.getIndexSearch()
		defer db.putIndexSearch(is)
		tfss := []*TagFilters{tfs}
		tr := TimeRange{
			MinTimestamp: 0,
			MaxTimestamp: timestampFromTime(time.Now()),
		}
		for i := 0; i < b.N; i++ {
			_, err := is.searchMetricIDs(tfss, tr, 2e9)
			if err != nil {
				b.Fatalf("unexpected error in searchMetricIDs: %s", err)
			}
		}
	}
	addTagFilter := func(tfs *TagFilters, key, value string, isNegative, isRegexp bool) {
		if err := tfs.Add([]byte(key), []byte(value), isNegative, isRegexp); err != nil {
			b.Fatalf("cannot add tag filter %q=%q, isNegative=%v, isRegexp=%v", key, value, isNegative, isRegexp)
		}
	}

	b.Run(`n="1"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",j="foo"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "j", "foo", false, false)
		benchSearch(b, tfs)
	})
	b.Run(`j="foo",n="1"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "j", "foo", false, false)
		addTagFilter(tfs, "n", "1", false, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",j!="foo"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "j", "foo", true, false)
		benchSearch(b, tfs)
	})
	b.Run(`i=~".*"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "i", ".*", false, true)
		benchSearch(b, tfs)
	})
	b.Run(`i=~".+"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "i", ".+", false, true)
		benchSearch(b, tfs)
	})
	b.Run(`i=~""`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "i", "", false, true)
		benchSearch(b, tfs)
	})
	b.Run(`i!=""`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "i", "", true, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",i=~".*",j="foo"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "i", ".*", false, true)
		addTagFilter(tfs, "j", "foo", false, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",i=~".*",i!="2",j="foo"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "i", ".*", false, true)
		addTagFilter(tfs, "i", "2", true, false)
		addTagFilter(tfs, "j", "foo", false, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",i!=""`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "i", "", true, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",i!="",j="foo"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "i", "", true, false)
		addTagFilter(tfs, "j", "foo", false, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",i=~".+",j="foo"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "i", ".+", false, true)
		addTagFilter(tfs, "j", "foo", false, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",i=~"1.+",j="foo"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "i", "1.+", false, true)
		addTagFilter(tfs, "j", "foo", false, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",i=~".+",i!="2",j="foo"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "i", ".+", false, true)
		addTagFilter(tfs, "i", "2", true, false)
		addTagFilter(tfs, "j", "foo", false, false)
		benchSearch(b, tfs)
	})
	b.Run(`n="1",i=~".+",i!~"2.*",j="foo"`, func(b *testing.B) {
		tfs := NewTagFilters(accountID, projectID)
		addTagFilter(tfs, "n", "1", false, false)
		addTagFilter(tfs, "i", ".+", false, true)
		addTagFilter(tfs, "i", "2.*", true, true)
		addTagFilter(tfs, "j", "foo", false, false)
		benchSearch(b, tfs)
	})
}

func BenchmarkIndexDBGetTSIDs(b *testing.B) {
	metricIDCache := workingsetcache.New(1234, time.Hour)
	metricNameCache := workingsetcache.New(1234, time.Hour)
	defer metricIDCache.Stop()
	defer metricNameCache.Stop()

	var hmCurr atomic.Value
	hmCurr.Store(&hourMetricIDs{})
	var hmPrev atomic.Value
	hmPrev.Store(&hourMetricIDs{})

	const dbName = "bench-index-db-get-tsids"
	db, err := openIndexDB(dbName, metricIDCache, metricNameCache, &hmCurr, &hmPrev)
	if err != nil {
		b.Fatalf("cannot open indexDB: %s", err)
	}
	defer func() {
		db.MustClose()
		if err := os.RemoveAll(dbName); err != nil {
			b.Fatalf("cannot remove indexDB: %s", err)
		}
	}()

	const recordsPerLoop = 1000
	const accountsCount = 111
	const projectsCount = 33333
	const recordsCount = 1e5

	// Fill the db with recordsCount records.
	var mn MetricName
	mn.MetricGroup = []byte("rps")
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("key_%d", i)
		value := fmt.Sprintf("value_%d", i)
		mn.AddTag(key, value)
	}
	var tsid TSID
	var metricName []byte

	is := db.getIndexSearch()
	defer db.putIndexSearch(is)
	for i := 0; i < recordsCount; i++ {
		mn.AccountID = uint32(i % accountsCount)
		mn.ProjectID = uint32(i % projectsCount)
		mn.sortTags()
		metricName = mn.Marshal(metricName[:0])
		if err := is.GetOrCreateTSIDByName(&tsid, metricName); err != nil {
			b.Fatalf("cannot insert record: %s", err)
		}
	}

	b.SetBytes(recordsPerLoop)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var tsidLocal TSID
		var metricNameLocal []byte
		mnLocal := mn
		is := db.getIndexSearch()
		defer db.putIndexSearch(is)
		for pb.Next() {
			for i := 0; i < recordsPerLoop; i++ {
				mnLocal.AccountID = uint32(i % accountsCount)
				mnLocal.ProjectID = uint32(i % projectsCount)
				mnLocal.sortTags()
				metricNameLocal = mnLocal.Marshal(metricNameLocal[:0])
				if err := is.GetOrCreateTSIDByName(&tsidLocal, metricNameLocal); err != nil {
					panic(fmt.Errorf("cannot obtain tsid: %s", err))
				}
			}
		}
	})
	b.StopTimer()
}
