/*
package apisrv provides an implementation of the gRPC server defined in ../../../api/protobuf-spec/mmlogic.proto.
Most of the documentation for what these calls should do is in that file!

Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

*/

package apisrv

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"

	mmlogic "github.com/GoogleCloudPlatform/open-match/cmd/mmlogicapi/proto"
	"github.com/GoogleCloudPlatform/open-match/internal/metrics"
	"github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis/ignorelist"
	"github.com/gogo/protobuf/jsonpb"
	log "github.com/sirupsen/logrus"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"

	"github.com/tidwall/gjson"

	"github.com/gomodule/redigo/redis"
	"github.com/spf13/viper"

	"go.opencensus.io/plugin/ocgrpc"
	"google.golang.org/grpc"
)

// Logrus structured logging setup
var (
	mlLogFields = log.Fields{
		"app":       "openmatch",
		"component": "mmlogic",
		"caller":    "mmlogicapi/apisrv/apisrv.go",
	}
	mlLog = log.WithFields(mlLogFields)
)

// MmlogicAPI implements mmlogic.ApiServer, the server generated by compiling
// the protobuf, by fulfilling the mmlogic.APIClient interface.
type MmlogicAPI struct {
	grpc *grpc.Server
	cfg  *viper.Viper
	pool *redis.Pool
}
type mmlogicAPI MmlogicAPI

// New returns an instantiated srvice
func New(cfg *viper.Viper, pool *redis.Pool) *MmlogicAPI {
	s := MmlogicAPI{
		pool: pool,
		grpc: grpc.NewServer(grpc.StatsHandler(&ocgrpc.ServerHandler{})),
		cfg:  cfg,
	}

	// Add a hook to the logger to auto-count log lines for metrics output thru OpenCensus
	log.AddHook(metrics.NewHook(MlLogLines, KeySeverity))

	// Register gRPC server
	mmlogic.RegisterAPIServer(s.grpc, (*mmlogicAPI)(&s))
	mlLog.Info("Successfully registered gRPC server")
	return &s
}

// Open opens the api grpc service, starting it listening on the configured port.
func (s *MmlogicAPI) Open() error {
	ln, err := net.Listen("tcp", ":"+s.cfg.GetString("api.mmlogic.port"))
	if err != nil {
		mlLog.WithFields(log.Fields{
			"error": err.Error(),
			"port":  s.cfg.GetInt("api.mmlogic.port"),
		}).Error("net.Listen() error")
		return err
	}
	mlLog.WithFields(log.Fields{"port": s.cfg.GetInt("api.mmlogic.port")}).Info("TCP net listener initialized")

	go func() {
		err := s.grpc.Serve(ln)
		if err != nil {
			mlLog.WithFields(log.Fields{"error": err.Error()}).Error("gRPC serve() error")
		}
		mlLog.Info("serving gRPC endpoints")
	}()

	return nil
}

func (s *mmlogicAPI) GetProposal(c context.Context, in *mmlogic.MatchObject) (*mmlogic.MatchObject, error) {
	return &mmlogic.MatchObject{}, nil
}

// GetProfile is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
func (s *mmlogicAPI) GetProfile(c context.Context, in *mmlogic.Profile) (*mmlogic.Profile, error) {

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	// Create context for tagging OpenCensus metrics.
	funcName := "GetProfile"
	fnCtx, _ := tag.New(c, tag.Insert(KeyMethod, funcName))

	mlLog.WithFields(log.Fields{"profileid": in.Id}).Info("Attempting retreival of profile")

	// Write group
	profile, err := redis.StringMap(redisConn.Do("HGETALL", in.Id))
	if err != nil {
		mlLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
			"profileid": in.Id,
		}).Error("State storage error")

		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return &mmlogic.Profile{Id: in.Id, Properties: ""}, err
	}
	out := &mmlogic.Profile{Id: in.Id, Properties: profile["properties"], PlayerPools: []*mmlogic.PlayerPool{}}
	mlLog.WithFields(log.Fields{"profileid": in.Id, "contents": profile}).Debug("Retrieved profile from state storage")

	// Unmarshal the player pools: the backend api writes the PlayerPools
	// protobuf message to redis by Marshalling it to a JSON string, so
	// Unmarshal here to get back a protobuf messages.
	ppools := gjson.Get(profile["playerPools"], "playerPools")
	for _, pool := range ppools.Array() {
		pp := mmlogic.PlayerPool{}
		err = jsonpb.UnmarshalString(pool.String(), &pp)
		if err != nil {
			mlLog.WithFields(log.Fields{
				"error":     err.Error(),
				"component": "jsonpb.UnmarshalString",
				"profileid": in.Id,
				"JSON":      pool.String(),
			}).Error("Failure to Unmarshal Player Pool JSON to protobuf message")
		}
		out.PlayerPools = append(out.PlayerPools, &pp)
	}

	mlLog.Debug(out)

	stats.Record(fnCtx, MlGrpcRequests.M(1))
	return out, err

}

// CreateProposal is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
func (s *mmlogicAPI) CreateProposal(c context.Context, prop *mmlogic.MatchObject) (*mmlogic.Result, error) {

	// Retreive configured redis keys.
	list := s.cfg.GetString("ignoreLists.proposedPlayers")
	proposalq := s.cfg.GetString("queues.proposals.name")

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	// Create context for tagging OpenCensus metrics.
	funcName := "CreateProposal"
	fnCtx, _ := tag.New(c, tag.Insert(KeyMethod, funcName))

	mlLog.Info("Attempting to create proposal")

	// update ignorelist
	playerIDs := make([]string, 0)
	for _, roster := range prop.Rosters {
		playerIDs = append(playerIDs, getPlayerIdsFromRoster(roster)...)
	}
	err := ignorelist.Update(redisConn, list, playerIDs)
	if err != nil {
		// TODO: update fields
		mlLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
			"key":       list,
		}).Error("State storage error")

		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return &mmlogic.Result{Success: false, Error: err.Error()}, err
	}

	// Write properties
	_, err = redisConn.Do("SET", prop.Id, prop.Properties)
	if err != nil {
		mlLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
			"key":       prop.Id,
		}).Error("State storage error")
		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return &mmlogic.Result{Success: false, Error: err.Error()}, err
	}
	/*
		_, err = redisConn.Do("SET", prop.Roster.Id, prop.Rosters.Players)
		if err != nil {
			mlLog.WithFields(log.Fields{
				"error":     err.Error(),
				"component": "statestorage",
				"key":       prop.Roster.Id,
			}).Error("State storage error")
			stats.Record(fnCtx, MlGrpcErrors.M(1))
			return &mmlogic.Result{Success: false, Error: err.Error()}, err
		}
	*/ // TODO: Fix after HSET conversion for matchobjects

	//  add propkey to proposalsq
	_, err = redisConn.Do("SADD", proposalq, prop.Id)
	if err != nil {
		mlLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
			"key":       proposalq,
		}).Error("State storage error")
		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return &mmlogic.Result{Success: false, Error: err.Error()}, err
	}

	stats.Record(fnCtx, MlGrpcRequests.M(1))
	return &mmlogic.Result{Success: true, Error: ""}, err

}

/*
// CreateMatchObject is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
func (s *mmlogicAPI) CreateResults(c context.Context, mmfr *mmlogic.MMFResults) (*mmlogic.Result, error) {

	list := s.cfg.GetString("ignoreLists.deindexedPlayers")

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	// Create context for tagging OpenCensus metrics.
	funcName := "TODO"
	fnCtx, _ := tag.New(c, tag.Insert(KeyMethod, funcName))

	mlLog.Info("Attempting to create match object with mmf results")

	// Write group
	playerIDs := getPlayerIdsFromRoster(mmfr.Roster)
	err := ignorelist.Update(redisConn, list, playerIDs)
	if err != nil {
		// TODO: update fields
		mlLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
			"key":       list,
		}).Error("State storage error")

		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return &mmlogic.Result{Success: false, Error: err.Error()}, err
	}

	// TODO: deindex
	// for playerID in mo.Roster.Profile
	//	go player.Deindex(playerID)

	// TODO: wite match to key

	// TODO: decrement the running MMFs

	return &mmlogic.Result{Success: true, Error: ""}, err
}
*/

// applyFilter is a sequential query of every entry in the Redis sorted set
// that fall beween the minimum and maximum values passed in through the filter
// argument.  This can be likely sped up later using concurrent access, but
// with small enough player pools (less than the 'redis.queryArgs.count' config
// parameter) the amount of work is identical, so this is fine as a starting point.
func (s *mmlogicAPI) applyFilter(c context.Context, filter *mmlogic.Filter) (map[string]int64, error) {

	f := filter.FilterSpec

	// Default maximum value is positive infinity (i.e. highest possible number in redis)
	// https://redis.io/commands/zrangebyscore
	maxv := strconv.FormatInt(f.Maxv, 10) // Convert int64 to a string
	if f.Maxv == 0 {                      // No max specified, set to +inf
		maxv = "+inf"
	}

	mlLog.WithFields(log.Fields{"filterField": f.Field}).Debug("In applyFilter")

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	// Check how many expected matches for this filter before we start retrieving.
	cmd := "ZCOUNT"
	count, err := redis.Int64(redisConn.Do(cmd, f.Field, f.Minv, maxv))
	countLog := mlLog.WithFields(log.Fields{
		"query": cmd,
		"field": f.Field,
		"minv":  f.Minv,
		"maxv":  maxv,
	})

	// Cases where the redis query succeeded but we don't want to process
	// the results.
	// 500,000 results is an arbitrary number; OM doesn't encourage
	// patterns where MMFs look at this large of a pool.
	if err == nil && (count == 0 || count > 500000) {
		err = errors.New("Number of player this filter applies to is either too large or too small, ignoring")
	}
	if err != nil {
		countLog.WithFields(log.Fields{"error": err.Error(), "count": count}).Error("statestorage error")
		return nil, err
	}

	// Cases where we continue.
	if count < 100000 {
		mlLog.WithFields(log.Fields{"count": count}).Info("number of players this filter applies to")
	} else {
		// Send a warning to the logs.
		mlLog.WithFields(log.Fields{"count": count}).Warn("number of players this filter applies to is very large")
	}

	// Amount of results look okay and no redis error, begin
	// var init for player retrieval
	cmd = "ZRANGEBYSCORE"
	offset := 0
	pool := make(map[string]int64)

	// Loop, retrieving players in chunks.
	for len(pool) == offset {
		results, err := redis.Int64Map(redisConn.Do(cmd, f.Field, f.Minv, maxv, "WITHSCORES", "LIMIT", offset, s.cfg.GetInt("redis.queryArgs.count")))
		if err != nil {
			mlLog.WithFields(log.Fields{
				"query":  cmd,
				"field":  f.Field,
				"minv":   f.Minv,
				"maxv":   maxv,
				"offset": offset,
				"count":  s.cfg.GetInt("redis.queryArgs.count"),
				"error":  err.Error(),
			}).Error("statestorage error")
		}

		// Increment the offset for the next query by the 'count' config value
		offset = offset + s.cfg.GetInt("redis.queryArgs.count")

		// Add all results to this player pool
		for k, v := range results {
			if _, ok := pool[k]; ok {
				// Redis returned the same player more than once; this is not
				// actually a problem, it just indicates that players are being
				// added/removed from the index as it is queried.  We take the
				// tradeoff in consistency for speed, as it won't cause issues
				// in matchmaking results as long as ignorelists are respected.
				offset--
			}
			pool[k] = v
		}
	}

	// Log completion and return
	mlLog.WithFields(log.Fields{
		"poolSize": len(pool),
		"field":    f.Field,
		"minv":     f.Minv,
		"maxv":     maxv,
	}).Info("Player pool filter processed")

	return pool, nil
}

// GetPlayerPool is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
// API_GetPlayerPoolServer returns mutiple PlayerPool messages - they should
// all be reassembled into one set on the calling side, as they are just
// paginated subsets of the player pool.
func (s *mmlogicAPI) GetPlayerPool(pool *mmlogic.PlayerPool, stream mmlogic.API_GetPlayerPoolServer) error {

	// TODO: quit if context is cancelled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One working Roster per filter in the set.  Combined at the end.
	filteredRosters := make(map[string][]string)
	// Temp store the results so we can also populate some field values in the final return roster.
	filteredResults := make(map[string]map[string]int64)
	overlap := make([]string, 0)
	fnStart := time.Now()

	// Loop over all filters, get results, combine
	for _, thisFilter := range pool.Filters {
		f := thisFilter.FilterSpec

		filterStart := time.Now()
		results, err := s.applyFilter(ctx, thisFilter)
		thisFilter.Stats = &mmlogic.Stats{Count: int64(len(results)), Elapsed: time.Since(filterStart).Seconds()}
		if err != nil {
			mlLog.WithFields(log.Fields{"error": err.Error(), "filterid": thisFilter.Id}).Debug("Error applying filter")

			if len(results) == 0 {
				// One simple optimization here: check the count returned by a
				// ZCOUNT query for each filter before doing anything.  If any of the
				// filters return a ZCOUNT of 0, then the logical AND of all filters will
				// container no players and we can shortcircuit and quit.
				mlLog.WithFields(log.Fields{
					"count":    0,
					"filterid": thisFilter.Id,
					"pool":     pool.Id,
				}).Info("This filter returned zero players. Returning empty pool")

				// Fill in the stats for this player pool.
				pool.Stats = &mmlogic.Stats{Count: int64(len(results)), Elapsed: time.Since(filterStart).Seconds()}
				// Return empty roster.
				pool.Roster = []*mmlogic.Roster{}

				// Send the empty pool and exit.
				if err = stream.Send(pool); err != nil {
					return err
				}
				return nil
			}

		}

		// Make an array of only the player IDs; used to do unions and find the
		// logical AND
		m := make([]string, len(results))
		i := 0
		for playerID := range results {
			m[i] = playerID
			i++
		}

		// Store the array of player IDs as well as the full results for later
		// retrieval
		filteredRosters[f.Field] = m
		filteredResults[f.Field] = results
		overlap = m
	}

	// Player must be in every filtered pool to be returned
	for field, thesePlayers := range filteredRosters {
		overlap = intersection(overlap, thesePlayers)

		mlLog.WithFields(log.Fields{"count": len(overlap), "field": field}).Debug("Amount of overlap")
		mlLog.WithFields(log.Fields{"field": field, "first10": overlap[:min(len(overlap), 10)]}).Debug("Sample of overlap")
	}

	// Get contents of all ignore lists and remove those players from the pool.
	il, err := s.allIgnoreLists(ctx, &mmlogic.IlInput{})
	if err != nil {
		mlLog.Error(err)
	}
	playerList := difference(overlap, il) // removes ignorelist from the Roster
	mlLog.WithFields(log.Fields{"count": len(overlap)}).Debug("Overlap size")
	mlLog.WithFields(log.Fields{"count": len(il)}).Debug("Ignorelist size")
	mlLog.WithFields(log.Fields{"count": len(playerList)}).Debug("Pool size")

	// Reformat the playerList as a gRPC PlayerPool message. Send partial results as we go.
	// This is pretty agressive in the partial result 'page'
	// sizes it sends, and that is partially because it assumes you're running
	// everything on a local network.  If you aren't, you may need to tune this
	// pageSize.
	pageSize := s.cfg.GetInt("redis.results.pageSize")
	pageCount := int(math.Ceil((float64(len(playerList)) / float64(pageSize)))) // Divides and rounds up on any remainder
	//TODO: change if removing filtersets from rosters in favor of it being in pools
	partialRoster := mmlogic.Roster{Id: fmt.Sprintf("%v.partialRoster", pool.Id)}
	pool.Stats = &mmlogic.Stats{Count: int64(len(playerList)), Elapsed: time.Since(fnStart).Seconds()}
	for i := 0; i < len(playerList); i++ {
		pID := playerList[i]
		player := &mmlogic.Player{Id: pID, Properties: []*mmlogic.Player_Property{}}

		// Loop through all results for all filtered fields, and add those values to this player
		for field, fr := range filteredResults {
			if value, ok := fr[pID]; ok {
				player.Properties = append(player.Properties, &mmlogic.Player_Property{Name: field, Value: value})
			}
		}
		partialRoster.Players = append(partialRoster.Players, player)

		// Check if we've filled in enough players to fill a page of results.
		if i%pageSize == 0 {
			pageName := fmt.Sprintf("%v.page%v%v", pool.Id, i/pageSize, pageCount)
			poolChunk := &mmlogic.PlayerPool{
				Id:      pageName,
				Filters: pool.Filters,
				Stats:   pool.Stats,
				Roster:  []*mmlogic.Roster{&partialRoster},
			}
			if err = stream.Send(poolChunk); err != nil {
				return err
			}
			partialRoster.Players = []*mmlogic.Player{}
		}
	}
	// TODO: send last page
	mlLog.WithFields(log.Fields{"count": len(overlap), "pool": pool.Id}).Debug("Player pool streaming complete")

	return nil
}

// ListIgnoredPlayers is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
func (s *mmlogicAPI) ListIgnoredPlayers(c context.Context, olderThan *mmlogic.Timestamp) (*mmlogic.Roster, error) {

	list := s.cfg.GetString("ignoreLists.proposedPlayers")

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	// Create context for tagging OpenCensus metrics.
	funcName := "ListIgnoredPlayers"
	fnCtx, _ := tag.New(c, tag.Insert(KeyMethod, funcName))

	mlLog.WithFields(log.Fields{"ignorelist": list}).Info("Attempting to get ignorelist")

	// retreive ignore list
	il, err := ignorelist.Retrieve(redisConn, list, olderThan.Ts)
	if err != nil {
		mlLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
			"key":       list,
		}).Error("State storage error")

		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return &mmlogic.Roster{}, err
	}
	// TODO: fix this
	mlLog.Debug(fmt.Sprintf("Retreival success %v", il))

	stats.Record(fnCtx, MlGrpcRequests.M(1))
	return createRosterfromPlayerIds(il), err
}

// GetAllIgnoredPlayers is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
// This is a wrapper around allIgnoreLists, and converts the []string return
// value of that function to a gRPC Roster message to send out over the wire.
func (s *mmlogicAPI) GetAllIgnoredPlayers(c context.Context, in *mmlogic.IlInput) (*mmlogic.Roster, error) {

	// Create context for tagging OpenCensus metrics.
	funcName := "GetAllIgnoredPlayers"
	fnCtx, _ := tag.New(c, tag.Insert(KeyMethod, funcName))

	il, err := s.allIgnoreLists(c, in)

	stats.Record(fnCtx, MlGrpcRequests.M(1))
	return createRosterfromPlayerIds(il), err
}

// Create Error is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
func (s *mmlogicAPI) ReturnError(c context.Context, in *mmlogic.Result) (*mmlogic.Result, error) {
	return &mmlogic.Result{}, nil
}

// allIgnoreLists combines all the ignore lists and returns them.
func (s *mmlogicAPI) allIgnoreLists(c context.Context, in *mmlogic.IlInput) (allIgnored []string, err error) {

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	mlLog.Info("Attempting to get and combine ignorelists")
	funcName := "allIgnoreLists"

	for _, il := range s.cfg.GetStringMapString("ignoreLists") {
		mlLog.Debug(funcName, " Found IL named ", il)
		thisIl, err := ignorelist.Retrieve(redisConn, il, time.Now().Unix())
		if err != nil {
			panic(err)
		}

		allIgnored := union(allIgnored, thisIl)

		mlLog.WithFields(log.Fields{"count": len(allIgnored), "ignorelist": il}).Debug("Amount of overlap (this should never decrease)")
		mlLog.WithFields(log.Fields{"ignorelist": il, "first10": allIgnored[:min(10, len(allIgnored))]}).Debug("Sample of overlap")

	}

	return allIgnored, err
}

func intersection(a []string, b []string) (out []string) {

	hash := make(map[string]bool)

	for _, v := range a {
		hash[v] = true
	}

	for _, v := range b {
		if _, found := hash[v]; found {
			out = append(out, v)
		}
	}

	return out

}

func union(a []string, b []string) (out []string) {

	hash := make(map[string]bool)

	// collect all values from input args
	for _, v := range a {
		hash[v] = true
	}

	for _, v := range b {
		hash[v] = true
	}

	// put values into string array
	for k := range hash {
		out = append(out, k)
	}

	return out

}

func difference(a []string, b []string) (out []string) {

	hash := make(map[string]bool)
	out = append([]string{}, a...)

	for _, v := range b {
		hash[v] = true
	}

	// Iterate through output, removing items found in b
	for i := 0; i < len(out); i++ {
		if _, found := hash[out[i]]; found {
			// Remove this element by moving the copying the last element of the
			// array to this index and then slicing off the last element.
			// https://stackoverflow.com/a/37335777/3113674
			out[i] = out[len(out)-1]
			out = out[:len(out)-1]
		}
	}

	return out
}

func getPlayerIdsFromRoster(r *mmlogic.Roster) []string {
	playerIDs := make([]string, 0)
	for _, p := range r.Players {
		playerIDs = append(playerIDs, p.Id)
	}
	return playerIDs

}

func createRosterfromPlayerIds(playerIDs []string) *mmlogic.Roster {

	players := make([]*mmlogic.Player, 0)
	for _, id := range playerIDs {
		players = append(players, &mmlogic.Player{Id: id})
	}
	return &mmlogic.Roster{Players: players}

}

func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}
