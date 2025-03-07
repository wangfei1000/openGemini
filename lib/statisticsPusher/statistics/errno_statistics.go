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

package statistics

import (
	"sync"

	"github.com/openGemini/openGemini/lib/logger"
)

const (
	errnoStatisticsName = "errno"
)

var stat = &ErrnoStat{}

type ErrnoStat struct {
	init bool

	tags   map[string]string
	fields map[string]interface{}
	data   map[string]int64
	mu     sync.Mutex
}

func NewErrnoStat() *ErrnoStat {
	return stat
}

func (s *ErrnoStat) Add(code string) {
	if !s.init {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stat.data[code]++
}

func (s *ErrnoStat) Init(tags map[string]string) {
	s.data = make(map[string]int64)
	s.fields = make(map[string]interface{})
	s.tags = make(map[string]string)
	AllocTagMap(s.tags, tags)
	logger.SetErrnoStatHandler(s.Add)
	s.init = true
}

func (s *ErrnoStat) Collect(buf []byte) ([]byte, error) {
	if !s.init {
		return buf, nil
	}

	data := s.getData()
	if len(data) == 0 {
		return buf, nil
	}

	for code, n := range data {
		s.tags["errno"] = code
		s.tags["module"] = code[1:3]
		s.fields["value"] = n
		buf = AddPointToBuffer(errnoStatisticsName, s.tags, s.fields, buf)
	}

	return buf, nil
}

func (s *ErrnoStat) getData() map[string]int64 {
	if len(s.data) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data := s.data
	s.data = make(map[string]int64)
	return data
}
