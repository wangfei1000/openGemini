/*
Copyright 2022 Huawei Cloud Computing Technologies Co., Ltd.

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
//nolint
package tsi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openGemini/openGemini/lib/cpu"
	"github.com/openGemini/openGemini/lib/kvstorage"
	"github.com/openGemini/openGemini/lib/metaclient"
	"github.com/openGemini/openGemini/lib/tracing"
	"github.com/openGemini/openGemini/open_src/github.com/savsgio/dictpool"
	"github.com/openGemini/openGemini/open_src/influx/influxql"
	"github.com/openGemini/openGemini/open_src/influx/meta"
	"github.com/openGemini/openGemini/open_src/influx/query"
	"github.com/openGemini/openGemini/open_src/vm/protoparser/influx"
)

const (
	defaultTSIDCacheSize = 128 << 20
	defaultSKeyCacheSize = 128 << 20
	defaultTagCacheSize  = 512 << 20
)

type IndexType int

const (
	MergeSet IndexType = iota

	Text

	defaultSeriesKeyLen = 64
)

const (
	KVDirName = "kv"
)

var (
	sequenceID = uint64(time.Now().Unix())
)

var seriesKeyPool = &sync.Pool{}

func getSeriesKeyBuf() []byte {
	v := seriesKeyPool.Get()
	if v != nil {
		return v.([]byte)
	}
	return make([]byte, 0, defaultSeriesKeyLen)
}

func putSeriesKeyBuf(buf []byte) {
	buf = buf[:0]
	seriesKeyPool.Put(buf)
}

type TagSetInfo struct {
	ref int64

	IDs        []uint64
	Filters    []influxql.Expr
	SeriesKeys [][]byte           // encoded series key
	TagsVec    []influx.PointTags // tags of all series
	key        []byte             // group by tag sets key
}

func (t *TagSetInfo) String() string {
	n := len(t.IDs)
	var builder strings.Builder
	for i := 0; i < n; i++ {
		builder.WriteString(fmt.Sprintf("%d -> %s\n", t.IDs[i], t.SeriesKeys[i]))
	}
	return builder.String()
}

func (t *TagSetInfo) Len() int { return len(t.IDs) }
func (t *TagSetInfo) Less(i, j int) bool {
	return bytes.Compare(t.SeriesKeys[i], t.SeriesKeys[j]) < 0
}
func (t *TagSetInfo) Swap(i, j int) {
	t.SeriesKeys[i], t.SeriesKeys[j] = t.SeriesKeys[j], t.SeriesKeys[i]
	t.IDs[i], t.IDs[j] = t.IDs[j], t.IDs[i]
	t.TagsVec[i], t.TagsVec[j] = t.TagsVec[j], t.TagsVec[i]
	t.Filters[i], t.Filters[j] = t.Filters[j], t.Filters[i]
}

func NewTagSetInfo() *TagSetInfo {
	return setPool.get()
}

func (t *TagSetInfo) reset() {
	t.ref = 0
	t.key = t.key[:0]
	t.IDs = t.IDs[:0]
	t.Filters = t.Filters[:0]
	t.TagsVec = t.TagsVec[:0]

	for i := range t.SeriesKeys {
		putSeriesKeyBuf(t.SeriesKeys[i])
	}
	t.SeriesKeys = t.SeriesKeys[:0]
}

func (t *TagSetInfo) Append(id uint64, seriesKey []byte, filter influxql.Expr, tags influx.PointTags) {
	t.IDs = append(t.IDs, id)
	t.Filters = append(t.Filters, filter)
	t.TagsVec = append(t.TagsVec, tags)
	t.SeriesKeys = append(t.SeriesKeys, seriesKey)
}

func (t *TagSetInfo) Ref() {
	atomic.AddInt64(&t.ref, 1)
}

func (t *TagSetInfo) Unref() {
	if atomic.AddInt64(&t.ref, -1) == 0 {
		t.release()
	}
}

func (t *TagSetInfo) release() {
	t.reset()
	setPool.put(t)
}

type tagSetInfoPool struct {
	cache chan *TagSetInfo
	pool  *sync.Pool
}

var (
	setPool = NewTagSetPool()
)

func NewTagSetPool() *tagSetInfoPool {
	n := cpu.GetCpuNum() * 2
	if n < 8 {
		n = 8
	}
	if n > 128 {
		n = 128
	}

	return &tagSetInfoPool{
		cache: make(chan *TagSetInfo, n),
		pool:  &sync.Pool{},
	}
}

func (p *tagSetInfoPool) put(set *TagSetInfo) {
	select {
	case p.cache <- set:
	default:
		p.pool.Put(set)
	}
}

func (p *tagSetInfoPool) get() (set *TagSetInfo) {
	const defaultElementNum = 64
	select {
	case set = <-p.cache:
		return
	default:
		v := p.pool.Get()
		if v != nil {
			return v.(*TagSetInfo)
		}
		return &TagSetInfo{
			ref: 0,
			key: make([]byte, 0, 32),

			IDs:        make([]uint64, 0, defaultElementNum),
			SeriesKeys: make([][]byte, 0, defaultElementNum),
			Filters:    make([]influxql.Expr, 0, defaultElementNum),
			TagsVec:    make([]influx.PointTags, 0, defaultElementNum),
		}
	}
}

type GroupSeries []*TagSetInfo

func (gs GroupSeries) Len() int           { return len(gs) }
func (gs GroupSeries) Less(i, j int) bool { return bytes.Compare(gs[i].key, gs[j].key) < 0 }
func (gs GroupSeries) Swap(i, j int) {
	gs[i], gs[j] = gs[j], gs[i]
}
func (gs GroupSeries) Reverse() {
	sort.Sort(sort.Reverse(gs))
	for index := range gs {
		tt := gs[index]
		for i, j := 0, tt.Len()-1; i < j; i, j = i+1, j-1 {
			tt.IDs[i], tt.IDs[j] = tt.IDs[j], tt.IDs[i]
			tt.Filters[i], tt.Filters[j] = tt.Filters[j], tt.Filters[i]
			tt.SeriesKeys[i], tt.SeriesKeys[j] = tt.SeriesKeys[j], tt.SeriesKeys[i]
			tt.TagsVec[i], tt.TagsVec[j] = tt.TagsVec[j], tt.TagsVec[i]
		}
	}
}
func (gs GroupSeries) SeriesCnt() int {
	var cnt int
	for i := range gs {
		cnt += gs[i].Len()
	}
	return cnt
}

type Index interface {
	CreateIndexIfNotExists(mmRows *dictpool.Dict) error
	GetSeriesIdBySeriesKey(key, name []byte) (uint64, error)
	SearchSeries(series [][]byte, name []byte, condition influxql.Expr, tr TimeRange) ([][]byte, error)
	SearchSeriesWithOpts(span *tracing.Span, name []byte, opt *query.ProcessorOptions) (GroupSeries, error)
	SeriesCardinality(name []byte, condition influxql.Expr, tr TimeRange) (uint64, error)
	SearchSeriesKeys(series [][]byte, name []byte, condition influxql.Expr) ([][]byte, error)
	SearchAllSeriesKeys() ([][]byte, error)
	SearchTagValues(name []byte, tagKeys [][]byte, condition influxql.Expr) ([][]string, error)
	SearchAllTagValues(tagKey []byte) (map[string]map[string]struct{}, error)
	SearchTagValuesCardinality(name, tagKey []byte) (uint64, error)

	// search
	GetPrimaryKeys(name []byte, opt *query.ProcessorOptions) ([]uint64, error)
	// delete
	GetDeletePrimaryKeys(name []byte, condition influxql.Expr, tr TimeRange) ([]uint64, error)

	SetIndexBuilder(builder *IndexBuilder)

	DeleteTSIDs(name []byte, condition influxql.Expr, tr TimeRange) error

	Path() string

	DebugFlush()
	Open() error
	Close() error
}

type Options struct {
	ident     *meta.IndexIdentifier
	path      string
	indexType IndexType
	endTime   time.Time
	duration  time.Duration
	kvStorage kvstorage.KVStorage
}

func (opts *Options) Ident(ident *meta.IndexIdentifier) *Options {
	opts.ident = ident
	return opts
}

func (opts *Options) Path(path string) *Options {
	opts.path = path
	return opts
}

func (opts *Options) IndexType(indexType IndexType) *Options {
	opts.indexType = indexType
	return opts
}

func (opts *Options) EndTime(endTime time.Time) *Options {
	opts.endTime = endTime
	return opts
}

func (opts *Options) Duration(duration time.Duration) *Options {
	opts.duration = duration
	return opts
}

func (opts *Options) KVStorage(storage kvstorage.KVStorage) *Options {
	opts.kvStorage = storage
	return opts
}

func NewIndex(opts *Options) (Index, error) {
	switch opts.indexType {
	case MergeSet:
		return NewMergeSetIndex(opts)
	default:
		return NewMergeSetIndex(opts)
	}
}

func GenerateUUID() uint64 {
	b := kbPool.Get()
	// first three bytes is big endian of logicClock
	b.B = append(b.B, byte(metaclient.LogicClock>>16))
	b.B = append(b.B, byte(metaclient.LogicClock>>8))
	b.B = append(b.B, byte(metaclient.LogicClock))

	// last five bytes is big endian of sequenceID
	id := atomic.AddUint64(&sequenceID, 1)
	b.B = append(b.B, byte(id>>32))
	b.B = append(b.B, byte(id>>24))
	b.B = append(b.B, byte(id>>16))
	b.B = append(b.B, byte(id>>8))
	b.B = append(b.B, byte(id))

	pid := binary.BigEndian.Uint64(b.B)
	kbPool.Put(b)

	return pid
}

type DumpInfo struct {
	ShowTagKeys        bool
	ShowTagValues      bool
	ShowTagValueSeries bool
	MeasurementFilter  *regexp.Regexp
	TagKeyFilter       *regexp.Regexp
	TagValueFilter     *regexp.Regexp
}
