// Copyright 2013, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vtgate

import (
	"flag"
	"fmt"
	"html/template"
	"sort"
	"sync"
	"time"

	log "github.com/golang/glog"

	"github.com/youtube/vitess/go/stats"
	"github.com/youtube/vitess/go/vt/health"
	"github.com/youtube/vitess/go/vt/topo"
)

var (
	srvTopoCacheTTL = flag.Duration("srv_topo_cache_ttl", 1*time.Second, "how long to use cached entries for topology")
)

const (
	queryCategory  = "query"
	cachedCategory = "cached"
	errorCategory  = "error"
)

// SrvTopoServer is a subset of topo.Server that only contains the serving
// graph read-only calls used by clients to resolve serving addresses.
type SrvTopoServer interface {
	GetSrvKeyspaceNames(cell string) ([]string, error)

	GetSrvKeyspace(cell, keyspace string) (*topo.SrvKeyspace, error)

	GetEndPoints(cell, keyspace, shard string, tabletType topo.TabletType) (*topo.EndPoints, error)
}

// ResilientSrvTopoServer is an implementation of SrvTopoServer based
// on another SrvTopoServer that uses a cache for two purposes:
// - limit the QPS to the underlying SrvTopoServer
// - return the last known value of the data if there is an error
type ResilientSrvTopoServer struct {
	topoServer SrvTopoServer
	cacheTTL   time.Duration
	counts     *stats.Counters

	// mutex protects the cache map itself, not the individual
	// values in the cache.
	mutex                 sync.Mutex
	srvKeyspaceNamesCache map[string]*srvKeyspaceNamesEntry
	srvKeyspaceCache      map[string]*srvKeyspaceEntry
	endPointsCache        map[string]*endPointsEntry
}

type srvKeyspaceNamesEntry struct {
	// unmutable values
	cell string

	// the mutex protects any access to this structure (read or write)
	mutex sync.Mutex

	insertionTime time.Time
	value         []string
	lastError     error
}

type srvKeyspaceEntry struct {
	// unmutable values
	cell     string
	keyspace string

	// the mutex protects any access to this structure (read or write)
	mutex sync.Mutex

	insertionTime time.Time
	value         *topo.SrvKeyspace
	lastError     error
}

type endPointsEntry struct {
	// unmutable values
	cell       string
	keyspace   string
	shard      string
	tabletType topo.TabletType

	// the mutex protects any access to this structure (read or write)
	mutex sync.Mutex

	insertionTime time.Time

	// value is the end points that were returned to the client.
	value *topo.EndPoints
	// originalValue is the end points that were returned from
	// the topology server.
	originalValue *topo.EndPoints
	lastError     error
}

// filterUnhealthyServers removes the unhealthy servers from the list,
// unless all servers are unhealthy, then it keeps them all.
func filterUnhealthyServers(endPoints *topo.EndPoints) *topo.EndPoints {

	// no endpoints, return right away
	if endPoints == nil || len(endPoints.Entries) == 0 {
		return endPoints
	}

	healthyEndPoints := make([]topo.EndPoint, 0, len(endPoints.Entries))
	for _, ep := range endPoints.Entries {
		// if we are behind on replication, we're not 100% healthy
		if ep.Health != nil && ep.Health[health.ReplicationLag] == health.ReplicationLagHigh {
			continue
		}

		healthyEndPoints = append(healthyEndPoints, ep)
	}

	// we have healthy guys, we return them
	if len(healthyEndPoints) > 0 {
		return &topo.EndPoints{Entries: healthyEndPoints}
	}

	// we only have unhealthy guys, return them
	return endPoints
}

// NewResilientSrvTopoServer creates a new ResilientSrvTopoServer
// based on the provided SrvTopoServer.
func NewResilientSrvTopoServer(base SrvTopoServer, counterName string) *ResilientSrvTopoServer {
	return &ResilientSrvTopoServer{
		topoServer: base,
		cacheTTL:   *srvTopoCacheTTL,
		counts:     stats.NewCounters(counterName),

		srvKeyspaceNamesCache: make(map[string]*srvKeyspaceNamesEntry),
		srvKeyspaceCache:      make(map[string]*srvKeyspaceEntry),
		endPointsCache:        make(map[string]*endPointsEntry),
	}
}

func (server *ResilientSrvTopoServer) GetSrvKeyspaceNames(cell string) ([]string, error) {
	server.counts.Add(queryCategory, 1)

	// find the entry in the cache, add it if not there
	key := cell
	server.mutex.Lock()
	entry, ok := server.srvKeyspaceNamesCache[key]
	if !ok {
		entry = &srvKeyspaceNamesEntry{
			cell: cell,
		}
		server.srvKeyspaceNamesCache[key] = entry
	}
	server.mutex.Unlock()

	// Lock the entry, and do everything holding the lock.  This
	// means two concurrent requests will only issue one
	// underlying query.
	entry.mutex.Lock()
	defer entry.mutex.Unlock()

	// If the entry is fresh enough, return it
	if time.Now().Sub(entry.insertionTime) < server.cacheTTL {
		return entry.value, entry.lastError
	}

	// not in cache or too old, get the real value
	result, err := server.topoServer.GetSrvKeyspaceNames(cell)
	if err != nil {
		if entry.insertionTime.IsZero() {
			server.counts.Add(errorCategory, 1)
			log.Errorf("GetSrvKeyspaceNames(%v) failed: %v (no cached value, caching and returning error)", cell, err)
		} else {
			server.counts.Add(cachedCategory, 1)
			log.Warningf("GetSrvKeyspaceNames(%v) failed: %v (returning cached value: %v %v)", cell, err, entry.value, entry.lastError)
			return entry.value, entry.lastError
		}
	}

	// save the value we got and the current time in the cache
	entry.insertionTime = time.Now()
	entry.value = result
	entry.lastError = err
	return result, err
}

func (server *ResilientSrvTopoServer) GetSrvKeyspace(cell, keyspace string) (*topo.SrvKeyspace, error) {
	server.counts.Add(queryCategory, 1)

	// find the entry in the cache, add it if not there
	key := cell + "." + keyspace
	server.mutex.Lock()
	entry, ok := server.srvKeyspaceCache[key]
	if !ok {
		entry = &srvKeyspaceEntry{
			cell:     cell,
			keyspace: keyspace,
		}
		server.srvKeyspaceCache[key] = entry
	}
	server.mutex.Unlock()

	// Lock the entry, and do everything holding the lock.  This
	// means two concurrent requests will only issue one
	// underlying query.
	entry.mutex.Lock()
	defer entry.mutex.Unlock()

	// If the entry is fresh enough, return it
	if time.Now().Sub(entry.insertionTime) < server.cacheTTL {
		return entry.value, entry.lastError
	}

	// not in cache or too old, get the real value
	result, err := server.topoServer.GetSrvKeyspace(cell, keyspace)
	if err != nil {
		if entry.insertionTime.IsZero() {
			server.counts.Add(errorCategory, 1)
			log.Errorf("GetSrvKeyspace(%v, %v) failed: %v (no cached value, caching and returning error)", cell, keyspace, err)
		} else {
			server.counts.Add(cachedCategory, 1)
			log.Warningf("GetSrvKeyspace(%v, %v) failed: %v (returning cached value: %v %v)", cell, keyspace, err, entry.value, entry.lastError)
			return entry.value, entry.lastError
		}
	}

	// save the value we got and the current time in the cache
	entry.insertionTime = time.Now()
	entry.value = result
	entry.lastError = err
	return result, err
}

func (server *ResilientSrvTopoServer) GetEndPoints(cell, keyspace, shard string, tabletType topo.TabletType) (*topo.EndPoints, error) {
	server.counts.Add(queryCategory, 1)

	// find the entry in the cache, add it if not there
	key := cell + "." + keyspace + "." + shard + "." + string(tabletType)
	server.mutex.Lock()
	entry, ok := server.endPointsCache[key]
	if !ok {
		entry = &endPointsEntry{
			cell:       cell,
			keyspace:   keyspace,
			shard:      shard,
			tabletType: tabletType,
		}
		server.endPointsCache[key] = entry
	}
	server.mutex.Unlock()

	// Lock the entry, and do everything holding the lock.  This
	// means two concurrent requests will only issue one
	// underlying query.
	entry.mutex.Lock()
	defer entry.mutex.Unlock()

	// If the entry is fresh enough, return it
	if time.Now().Sub(entry.insertionTime) < server.cacheTTL {
		return entry.value, entry.lastError
	}

	// not in cache or too old, get the real value
	result, err := server.topoServer.GetEndPoints(cell, keyspace, shard, tabletType)
	if err != nil {
		if entry.insertionTime.IsZero() {
			server.counts.Add(errorCategory, 1)
			log.Errorf("GetEndPoints(%v, %v, %v, %v) failed: %v (no cached value, caching and returning error)", cell, keyspace, shard, tabletType, err)
		} else {
			server.counts.Add(cachedCategory, 1)
			log.Warningf("GetEndPoints(%v, %v, %v, %v) failed: %v (returning cached value: %v %v)", cell, keyspace, shard, tabletType, err, entry.value, entry.lastError)
			return entry.value, entry.lastError
		}
	}

	// save the value we got and the current time in the cache
	entry.insertionTime = time.Now()
	entry.originalValue = result
	entry.value = filterUnhealthyServers(result)
	entry.lastError = err

	return entry.value, err
}

// HealthyEndpointCount returns how many valid endpoints we have in the cache
func (server *ResilientSrvTopoServer) HealthyEndpointCount() map[string]int64 {
	result := make(map[string]int64)
	server.mutex.Lock()
	defer server.mutex.Unlock()
	for k, entry := range server.endPointsCache {
		entry.mutex.Lock()
		vl := int64(0)
		if entry.value != nil {
			vl = int64(len(entry.value.Entries))
		}
		entry.mutex.Unlock()
		result[k] = vl
	}
	return result
}

// DegradedEndpointCount returns how many degraded endpoints we have
// in the cache (entries that are not 100% healthy, because they are behind
// on replication for instance)
func (server *ResilientSrvTopoServer) DegradedEndpointCount() map[string]int64 {
	result := make(map[string]int64)
	server.mutex.Lock()
	defer server.mutex.Unlock()
	for k, entry := range server.endPointsCache {
		entry.mutex.Lock()
		// originalValue and value can be nil in case of error
		ovl := 0
		if entry.originalValue != nil {
			ovl = len(entry.originalValue.Entries)
		}
		vl := 0
		if entry.value != nil {
			vl = len(entry.value.Entries)
		}
		entry.mutex.Unlock()
		result[k] = int64(ovl - vl)
	}
	return result
}

// The next few structures and methods are used to get a displayable
// version of the cache in a status page

// SrvKeyspaceNamesCacheStatus is the current value for SrvKeyspaceNames
type SrvKeyspaceNamesCacheStatus struct {
	Cell      string
	Value     []string
	LastError error
}

// SrvKeyspaceNamesCacheStatusList is used for sorting
type SrvKeyspaceNamesCacheStatusList []*SrvKeyspaceNamesCacheStatus

// Len is part of sort.Interface
func (skncsl SrvKeyspaceNamesCacheStatusList) Len() int {
	return len(skncsl)
}

// Less is part of sort.Interface
func (skncsl SrvKeyspaceNamesCacheStatusList) Less(i, j int) bool {
	return skncsl[i].Cell < skncsl[j].Cell
}

// Swap is part of sort.Interface
func (skncsl SrvKeyspaceNamesCacheStatusList) Swap(i, j int) {
	skncsl[i], skncsl[j] = skncsl[j], skncsl[i]
}

// SrvKeyspaceCacheStatus is the current value for a SrvKeyspace object
type SrvKeyspaceCacheStatus struct {
	Cell      string
	Keyspace  string
	Value     *topo.SrvKeyspace
	LastError error
}

// StatusAsHTML returns an HTML version of our status.
// It works best if there is data in the cache.
func (st *SrvKeyspaceCacheStatus) StatusAsHTML() template.HTML {
	if st.Value == nil {
		return template.HTML("No Data")
	}

	result := "<b>Partitions:</b><br>"
	for tabletType, keyspacePartition := range st.Value.Partitions {
		result += "&nbsp;<b>" + string(tabletType) + "</b>"
		for _, shard := range keyspacePartition.Shards {
			result += "&nbsp;" + shard.ShardName()
		}
		result += "<br>"
	}

	result += "<b>TabletTypes:</b>"
	for _, tabletType := range st.Value.TabletTypes {
		result += "&nbsp;" + string(tabletType)
	}
	result += "<br>"

	if st.Value.ShardingColumnName != "" {
		result += "<b>ShardingColumnName:</b>&nbsp;" + st.Value.ShardingColumnName + "<br>"
		result += "<b>ShardingColumnType:</b>&nbsp;" + string(st.Value.ShardingColumnType) + "<br>"
	}

	if len(st.Value.ServedFrom) > 0 {
		result += "<b>ServedFrom:</b><br>"
		for tabletType, keyspace := range st.Value.ServedFrom {
			result += "&nbsp;<b>" + string(tabletType) + "</b>&nbsp;" + keyspace + "<br>"
		}
	}

	return template.HTML(result)
}

// SrvKeyspaceCacheStatusList is used for sorting
type SrvKeyspaceCacheStatusList []*SrvKeyspaceCacheStatus

// Len is part of sort.Interface
func (skcsl SrvKeyspaceCacheStatusList) Len() int {
	return len(skcsl)
}

// Less is part of sort.Interface
func (skcsl SrvKeyspaceCacheStatusList) Less(i, j int) bool {
	return skcsl[i].Cell+"."+skcsl[i].Keyspace <
		skcsl[j].Cell+"."+skcsl[j].Keyspace
}

// Swap is part of sort.Interface
func (skcsl SrvKeyspaceCacheStatusList) Swap(i, j int) {
	skcsl[i], skcsl[j] = skcsl[j], skcsl[i]
}

// EndPointsCacheStatus is the current value for an EndPoints object
type EndPointsCacheStatus struct {
	Cell          string
	Keyspace      string
	Shard         string
	TabletType    topo.TabletType
	Value         *topo.EndPoints
	OriginalValue *topo.EndPoints
	LastError     error
}

// StatusAsHTML returns an HTML version of our status.
// It works best if there is data in the cache.
func (st *EndPointsCacheStatus) StatusAsHTML() template.HTML {
	ovl := 0
	if st.OriginalValue != nil {
		ovl = len(st.OriginalValue.Entries)
	}
	vl := 0
	if st.Value != nil {
		vl = len(st.Value.Entries)
	}
	if ovl == vl {
		if vl == 0 {
			return template.HTML("<b>No entries</b>")
		}
		if len(st.OriginalValue.Entries[0].Health) > 0 {
			return template.HTML(fmt.Sprintf("<b>All %v values are unhappy</b>", vl))
		}
		return template.HTML(fmt.Sprintf("%v values are happy", vl))
	}
	return template.HTML(fmt.Sprintf("%v out of %v values are happy", vl, ovl))
}

// EndPointsCacheStatusList is used for sorting
type EndPointsCacheStatusList []*EndPointsCacheStatus

// Len is part of sort.Interface
func (epcsl EndPointsCacheStatusList) Len() int {
	return len(epcsl)
}

// Less is part of sort.Interface
func (epcsl EndPointsCacheStatusList) Less(i, j int) bool {
	return epcsl[i].Cell+"."+epcsl[i].Keyspace+"."+epcsl[i].Shard+"."+string(epcsl[i].TabletType) <
		epcsl[j].Cell+"."+epcsl[j].Keyspace+"."+epcsl[j].Shard+"."+string(epcsl[j].TabletType)
}

// Swap is part of sort.Interface
func (epcsl EndPointsCacheStatusList) Swap(i, j int) {
	epcsl[i], epcsl[j] = epcsl[j], epcsl[i]
}

// ResilientSrvTopoServerCacheStatus has the full status of the cache
type ResilientSrvTopoServerCacheStatus struct {
	SrvKeyspaceNames SrvKeyspaceNamesCacheStatusList
	SrvKeyspaces     SrvKeyspaceCacheStatusList
	EndPoints        EndPointsCacheStatusList
}

// CacheStatus returns a displayable version of the cache
func (server *ResilientSrvTopoServer) CacheStatus() *ResilientSrvTopoServerCacheStatus {
	result := &ResilientSrvTopoServerCacheStatus{}
	server.mutex.Lock()

	for _, entry := range server.srvKeyspaceNamesCache {
		entry.mutex.Lock()
		result.SrvKeyspaceNames = append(result.SrvKeyspaceNames, &SrvKeyspaceNamesCacheStatus{
			Cell:      entry.cell,
			Value:     entry.value,
			LastError: entry.lastError,
		})
		entry.mutex.Unlock()
	}

	for _, entry := range server.srvKeyspaceCache {
		entry.mutex.Lock()
		result.SrvKeyspaces = append(result.SrvKeyspaces, &SrvKeyspaceCacheStatus{
			Cell:      entry.cell,
			Keyspace:  entry.keyspace,
			Value:     entry.value,
			LastError: entry.lastError,
		})
		entry.mutex.Unlock()
	}

	for _, entry := range server.endPointsCache {
		entry.mutex.Lock()
		result.EndPoints = append(result.EndPoints, &EndPointsCacheStatus{
			Cell:          entry.cell,
			Keyspace:      entry.keyspace,
			Shard:         entry.shard,
			TabletType:    entry.tabletType,
			Value:         entry.value,
			OriginalValue: entry.originalValue,
			LastError:     entry.lastError,
		})
		entry.mutex.Unlock()
	}

	server.mutex.Unlock()

	// do the sorting without the mutex
	sort.Sort(result.SrvKeyspaceNames)
	sort.Sort(result.SrvKeyspaces)
	sort.Sort(result.EndPoints)

	return result
}
