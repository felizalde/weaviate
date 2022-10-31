//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2022 SeMI Technologies B.V. All rights reserved.
//
//  CONTACT: hello@semi.technology
//

package diskAnn

import (
	"context"
	"encoding/csv"
	"encoding/gob"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"
	ssdhelpers "github.com/semi-technologies/weaviate/adapters/repos/db/vector/ssdHelpers"
)

type Stats struct {
	Hops int
}

type VamanaData struct {
	SIndex          uint64 // entry point
	GraphID         string
	CachedEdges     map[uint64]*ssdhelpers.VectorWithNeighbors
	EncondedVectors [][]byte
	OnDisk          bool
}

type Vamana struct {
	config Config // configuration
	data   VamanaData

	cachedBitMap     *ssdhelpers.BitSet
	edges            [][]uint64 // edges on the graph
	set              ssdhelpers.Set
	graphFile        *os.File
	pq               *ssdhelpers.ProductQuantizer
	outNeighbors     func(uint64) ([]uint64, []float32)
	addRange         func([]uint64)
	beamSearchHolder func(*Vamana)
}

const ConfigFileName = "cfg.gob"
const DataFileName = "data.gob"
const GraphFileName = "graph.gob"

func New(config Config) (*Vamana, error) {
	index := &Vamana{
		config: config,
	}
	index.set = *ssdhelpers.NewSet(config.L, config.VectorForIDThunk, config.Distance, nil, int(config.VectorsSize))
	index.outNeighbors = index.outNeighborsFromMemory
	index.addRange = index.addRangeVectors
	index.beamSearchHolder = secuentialBeamSearch
	return index, nil
}

func BuildVamana(R int, L int, alpha float32, VectorForIDThunk ssdhelpers.VectorForID, vectorsSize uint64, distance ssdhelpers.DistanceFunction, path string) *Vamana {
	completePath := fmt.Sprintf("%s/%d.vamana-r%d-l%d-a%.1f", path, vectorsSize, R, L, alpha)
	if _, err := os.Stat(completePath); err == nil {
		return VamanaFromDisk(completePath, VectorForIDThunk, distance)
	}

	index, _ := New(Config{
		R:                  R,
		L:                  L,
		Alpha:              alpha,
		VectorForIDThunk:   VectorForIDThunk,
		VectorsSize:        vectorsSize,
		Distance:           distance,
		ClustersSize:       40,
		ClusterOverlapping: 2,
	})

	os.Mkdir(path, os.ModePerm)

	index.BuildIndex()
	index.ToDisk(completePath)
	index.beamSearchHolder = secuentialBeamSearch
	return index
}

func (v *Vamana) SetCacheSize(size int) {
	v.config.C = size
}

func (v *Vamana) SetBeamSize(size int) {
	v.config.BeamSize = size
}

func (v *Vamana) BuildIndexSharded() {
	if v.config.ClustersSize == 1 {
		v.BuildIndex()
		return
	}

	cluster := ssdhelpers.New(v.config.ClustersSize, v.config.Distance, v.config.VectorForIDThunk, int(v.config.VectorsSize), v.config.Dimensions)
	cluster.Partition()
	shards := make([][]uint64, v.config.ClustersSize)
	for i := 0; i < int(v.config.VectorsSize); i++ {
		i64 := uint64(i)
		vec, _ := v.config.VectorForIDThunk(context.Background(), i64)
		c := cluster.NNearest(vec, v.config.ClusterOverlapping)
		for j := 0; j < v.config.ClusterOverlapping; j++ {
			shards[c[j]] = append(shards[c[j]], i64)
		}
	}

	vectorForIDThunk := v.config.VectorForIDThunk
	vectorsSize := v.config.VectorsSize
	shardedGraphs := make([][][]uint64, v.config.ClustersSize)

	ssdhelpers.Concurrently(uint64(len(shards)), func(workerId, taskIndex uint64, mutex *sync.Mutex) {
		config := Config{
			R:     v.config.R,
			L:     v.config.L,
			Alpha: v.config.Alpha,
			VectorForIDThunk: func(ctx context.Context, id uint64) ([]float32, error) {
				return vectorForIDThunk(ctx, shards[taskIndex][id])
			},
			VectorsSize:        uint64(len(shards[taskIndex])),
			Distance:           v.config.Distance,
			ClustersSize:       v.config.ClustersSize,
			ClusterOverlapping: v.config.ClusterOverlapping,
		}

		index, _ := New(config)
		index.BuildIndex()
		shardedGraphs[taskIndex] = index.edges
	})

	v.config.VectorForIDThunk = vectorForIDThunk
	v.config.VectorsSize = vectorsSize
	v.data.SIndex = v.medoid()
	v.edges = make([][]uint64, v.config.VectorsSize)
	for shardIndex, shard := range shards {
		for connectionIndex, connection := range shardedGraphs[shardIndex] {
			for _, outNeighbor := range connection {
				mappedOutNeighbor := shard[outNeighbor]
				if !ssdhelpers.Contains(v.edges[shard[connectionIndex]], mappedOutNeighbor) {
					v.edges[shard[connectionIndex]] = append(v.edges[shard[connectionIndex]], mappedOutNeighbor)
				}
			}
		}
	}
	for edgeIndex := range v.edges {
		if len(v.edges[edgeIndex]) > v.config.R {
			if len(v.edges[edgeIndex]) > v.config.R {
				rand.Shuffle(len(v.edges[edgeIndex]), func(x int, y int) {
					temp := v.edges[edgeIndex][x]
					v.edges[edgeIndex][x] = v.edges[edgeIndex][y]
					v.edges[edgeIndex][y] = temp
				})
				//Meet the R constrain after merging
				//Take a random subset with the appropriate size. Implementation idea from Microsoft reference code
				v.edges[edgeIndex] = v.edges[edgeIndex][:v.config.R]
			}
		}
	}
}

func (v *Vamana) BuildIndex() {
	v.SetL(v.config.L)
	v.edges = v.makeRandomGraph()
	v.data.SIndex = v.medoid()
	alpha := v.config.Alpha
	v.config.Alpha = 1
	v.pass() //Not sure yet what did they mean in the paper with two passes... Two passes is exactly the same as only the last pass to the best of my knowledge.
	v.config.Alpha = alpha
	v.pass()
}

func (v *Vamana) GetGraph() [][]uint64 {
	return v.edges
}

func (v *Vamana) GetEntry() uint64 {
	return v.data.SIndex
}

func (v *Vamana) SetL(L int) {
	v.config.L = L
	v.set = *ssdhelpers.NewSet(L, v.config.VectorForIDThunk, v.config.Distance, nil, int(v.config.VectorsSize))
	v.set.SetPQ(v.data.EncondedVectors, v.pq)
}

func (v *Vamana) SearchByVector(query []float32, k int) []uint64 {
	return v.greedySearchQuery(query, k)
}

func (v *Vamana) ToDisk(path string) {
	fConfig, err := os.Create(fmt.Sprintf("%s/%s", path, ConfigFileName))
	if err != nil {
		panic(errors.Wrap(err, "Could not create config file"))
	}
	fData, err := os.Create(fmt.Sprintf("%s/%s", path, DataFileName))
	if err != nil {
		panic(errors.Wrap(err, "Could not create entry point file"))
	}
	fGraph, err := os.Create(fmt.Sprintf("%s/%s", path, GraphFileName))
	if err != nil {
		panic(errors.Wrap(err, "Could not create graph file"))
	}
	defer fConfig.Close()
	defer fData.Close()
	defer fGraph.Close()

	cEnc := gob.NewEncoder(fConfig)
	err = cEnc.Encode(v.config)
	if err != nil {
		panic(errors.Wrap(err, "Could not encode config"))
	}

	dEnc := gob.NewEncoder(fData)
	err = dEnc.Encode(v.data)
	if err != nil {
		panic(errors.Wrap(err, "Could not encode data"))
	}

	gEnc := gob.NewEncoder(fGraph)
	err = gEnc.Encode(v.edges)
	if err != nil {
		panic(errors.Wrap(err, "Could not encode graph"))
	}

	v.pq.ToDisk(path)
	v.cachedBitMap.ToDisk(path)
}

func (v *Vamana) GraphFromDumpFile(filePath string) {
	f, err := os.Open(filePath)
	if err != nil {
		panic(errors.Wrap(err, "Unable to read input file "+filePath))
	}
	defer f.Close()
	csvReader := csv.NewReader(f)
	csvReader.FieldsPerRecord = -1
	records, err := csvReader.ReadAll()
	if err != nil {
		panic(errors.Wrap(err, "Unable to parse file as CSV for "+filePath))
	}
	v.edges = make([][]uint64, v.config.VectorsSize)
	for r, row := range records {
		v.edges[r] = make([]uint64, len(row)-1)
		for j, element := range row {
			if j == len(row)-1 {
				break
			}
			v.edges[r][j] = str2uint64(element)
		}
	}
}

func str2uint64(str string) uint64 {
	str = strings.Trim(str, " ")
	i, _ := strconv.ParseInt(str, 10, 64)
	return uint64(i)
}

func VamanaFromDisk(path string, VectorForIDThunk ssdhelpers.VectorForID, distance ssdhelpers.DistanceFunction) *Vamana {
	fConfig, err := os.Open(fmt.Sprintf("%s/%s", path, ConfigFileName))
	if err != nil {
		panic(errors.Wrap(err, "Could not open config file"))
	}
	fData, err := os.Open(fmt.Sprintf("%s/%s", path, DataFileName))
	if err != nil {
		panic(errors.Wrap(err, "Could not open entry point file"))
	}
	fGraph, err := os.Open(fmt.Sprintf("%s/%s", path, GraphFileName))
	if err != nil {
		panic(errors.Wrap(err, "Could not open graph file"))
	}
	defer fConfig.Close()
	defer fData.Close()
	defer fGraph.Close()

	var config Config
	cDec := gob.NewDecoder(fConfig)
	err = cDec.Decode(&config)
	config.Dimensions = 128
	if err != nil {
		panic(errors.Wrap(err, "Could not decode config"))
	}

	index, err := New(config)

	dDec := gob.NewDecoder(fData)
	err = dDec.Decode(&index.data)
	if err != nil {
		panic(errors.Wrap(err, "Could not decode data"))
	}

	gDec := gob.NewDecoder(fGraph)
	err = gDec.Decode(&index.edges)
	if err != nil {
		panic(errors.Wrap(err, "Could not decode edges"))
	}
	index.config.VectorForIDThunk = VectorForIDThunk
	index.config.Distance = distance
	if index.data.OnDisk && index.config.BeamSize > 1 {
		index.beamSearchHolder = initBeamSearch
	} else {
		index.beamSearchHolder = secuentialBeamSearch
	}
	index.pq = ssdhelpers.PQFromDisk(path, VectorForIDThunk, distance)
	index.cachedBitMap = ssdhelpers.BitSetFromDisk(path)
	if index.data.OnDisk {
		index.outNeighbors = index.OutNeighborsFromDisk
		index.addRange = index.addRangePQ
		index.graphFile, _ = os.Open(index.data.GraphID)
	} else {
		index.outNeighbors = index.outNeighborsFromMemory
		index.addRange = index.addRangeVectors
	}
	return index
}

func (v *Vamana) pass() {
	random_order := permutation(int(v.config.VectorsSize))
	for i := range random_order {
		x := random_order[i]
		x64 := uint64(x)
		q, err := v.config.VectorForIDThunk(context.Background(), x64)
		if err != nil {
			panic(errors.Wrap(err, fmt.Sprintf("Could not fetch vector with id %d", x64)))
		}
		_, visited := v.greedySearch(q, 1)
		v.robustPrune(x64, visited)
		n_out_i := v.edges[x]
		for j := range n_out_i {
			n_out_j := append(v.edges[n_out_i[j]], x64)
			if len(n_out_j) > v.config.R {
				v.robustPrune(n_out_i[j], n_out_j)
			} else {
				v.edges[n_out_i[j]] = n_out_j
			}
		}
	}
}

func min(x uint64, y uint64) uint64 {
	if x < y {
		return x
	}
	return y
}

func (v *Vamana) makeRandomGraph() [][]uint64 {
	edges := make([][]uint64, v.config.VectorsSize)
	ssdhelpers.Concurrently(v.config.VectorsSize, func(workerID uint64, i uint64, mutex *sync.Mutex) {
		edges[i] = make([]uint64, v.config.R)
		for j := 0; j < v.config.R; j++ {
			edges[i][j] = rand.Uint64() % (v.config.VectorsSize - 1)
			if edges[i][j] >= i { //avoid connecting with itself
				edges[i][j]++
			}
		}
	})
	return edges
}

func (v *Vamana) medoid() uint64 {
	var min_dist float32 = math.MaxFloat32
	min_index := uint64(0)

	mean := make([]float32, v.config.VectorsSize)
	for i := uint64(0); i < v.config.VectorsSize; i++ {
		x, err := v.config.VectorForIDThunk(context.Background(), i)
		if err != nil {
			panic(errors.Wrap(err, fmt.Sprintf("Could not fetch vector with id %d", i)))
		}
		for j := 0; j < len(x); j++ {
			mean[j] += x[j]
		}
	}
	for j := 0; j < len(mean); j++ {
		mean[j] /= float32(v.config.VectorsSize)
	}

	//ToDo: Not really helping like this
	ssdhelpers.Concurrently(v.config.VectorsSize, func(workerID uint64, i uint64, mutex *sync.Mutex) {
		x, err := v.config.VectorForIDThunk(context.Background(), i)
		if err != nil {
			panic(errors.Wrap(err, fmt.Sprintf("Could not fetch vector with id %d", i)))
		}
		dist := v.config.Distance(x, mean)
		mutex.Lock()
		if dist < min_dist {
			min_dist = dist
			min_index = uint64(i)
		}
		mutex.Unlock()
	})
	return min_index
}

func permutation(n int) []int {
	permutation := make([]int, n)
	for i := range permutation {
		permutation[i] = i
	}
	for i := 0; i < 2*n; i++ {
		x := rand.Intn(n)
		y := rand.Intn(n)
		z := permutation[x]
		permutation[x] = permutation[y]
		permutation[y] = z
	}
	return permutation
}

func (v *Vamana) greedySearch(x []float32, k int) ([]uint64, []uint64) {
	v.set.ReCenter(x)
	v.set.Add(v.data.SIndex)
	allVisited := []uint64{v.data.SIndex}
	for v.set.NotVisited() {
		nn, _ := v.set.Top()
		v.set.AddRange(v.edges[nn])
		allVisited = append(allVisited, nn)
	}
	return v.set.Elements(k), allVisited
}

func (v *Vamana) addRangeVectors(elements []uint64) {
	v.set.AddRange(elements)
}

func (v *Vamana) addRangePQ(elements []uint64) {
	v.set.AddRangePQ(elements, v.data.CachedEdges, v.cachedBitMap)
}

func initBeamSearch(v *Vamana) {
	secuentialBeamSearch(v)
	v.beamSearchHolder = beamSearch
}

func beamSearch(v *Vamana) {
	tops, indexes := v.set.TopN(v.config.BeamSize)
	neighbours := make([][]uint64, v.config.BeamSize)
	vectors := make([][]float32, v.config.BeamSize)
	ssdhelpers.Concurrently(uint64(len(tops)), func(workerId, i uint64, mutex *sync.Mutex) {
		neighbours[i], vectors[i] = v.outNeighbors(tops[i])
	})
	for i := range indexes {
		if vectors[i] != nil {
			v.set.ReSort(indexes[i], vectors[i])
		}
		v.addRange(neighbours[i])
	}
}

func secuentialBeamSearch(v *Vamana) {
	top, index := v.set.Top()
	neighbours, vector := v.outNeighbors(top)
	if vector != nil {
		v.set.ReSort(index, vector)
	}
	v.addRange(neighbours)
}

func (v *Vamana) greedySearchQuery(x []float32, k int) []uint64 {
	v.set.ReCenter(x)
	if v.data.OnDisk {
		v.set.AddPQVector(v.data.SIndex, v.data.CachedEdges, v.cachedBitMap)
	} else {
		v.set.Add(v.data.SIndex)
	}

	for v.set.NotVisited() {
		v.beamSearchHolder(v)
	}
	if v.data.OnDisk && v.config.BeamSize > 1 {
		v.beamSearchHolder = initBeamSearch
	}
	return v.set.Elements(k)
}

func (v *Vamana) outNeighborsFromMemory(x uint64) ([]uint64, []float32) {
	return v.edges[x], nil
}

func (v *Vamana) OutNeighborsFromDisk(x uint64) ([]uint64, []float32) {
	cached, found := v.data.CachedEdges[x]
	if found {
		return cached.OutNeighbors, nil
	}
	return ssdhelpers.ReadGraphRowWithBinary(v.graphFile, x, v.config.R, v.config.Dimensions)
}

func (v *Vamana) addToCacheRecursively(hops int, elements []uint64) {
	if hops <= 0 {
		return
	}

	newElements := make([]uint64, 0)
	for _, x := range elements {
		if hops <= 0 {
			return
		}
		found := v.cachedBitMap.ContainsAndAdd(x)
		if found {
			continue
		}
		hops--

		vec, _ := v.config.VectorForIDThunk(context.Background(), uint64(x))
		v.data.CachedEdges[x] = &ssdhelpers.VectorWithNeighbors{
			Vector:       vec,
			OutNeighbors: v.edges[x],
		}
		for _, n := range v.edges[x] {
			newElements = append(newElements, n)
		}
	}
	v.addToCacheRecursively(hops, newElements)
}

func (v *Vamana) SwitchGraphToDisk(path string, segments int, centroids int) {
	v.data.GraphID = path
	ssdhelpers.DumpGraphToDiskWithBinary(v.data.GraphID, v.edges, v.config.R, v.config.VectorForIDThunk, v.config.Dimensions)
	v.outNeighbors = v.OutNeighborsFromDisk
	v.data.CachedEdges = make(map[uint64]*ssdhelpers.VectorWithNeighbors, v.config.C)
	v.cachedBitMap = ssdhelpers.NewBitSet(int(v.config.VectorsSize))
	v.addToCacheRecursively(v.config.C, []uint64{v.data.SIndex})
	v.edges = nil
	v.graphFile, _ = os.Open(v.data.GraphID)
	v.data.EncondedVectors = v.encondeVectors(segments, centroids)
	v.set.SetPQ(v.data.EncondedVectors, v.pq)
	v.addRange = v.addRangePQ
	v.data.OnDisk = true
	if v.config.BeamSize > 1 {
		v.beamSearchHolder = initBeamSearch
	}
}

func (v *Vamana) encondeVectors(segments int, centroids int) [][]byte {
	v.pq = ssdhelpers.NewProductQunatizer(segments, centroids, v.config.Distance, v.config.VectorForIDThunk, v.config.Dimensions, int(v.config.VectorsSize))
	v.pq.Fit()
	enconded := make([][]byte, v.config.VectorsSize)
	ssdhelpers.Concurrently(v.config.VectorsSize, func(workerID uint64, vIndex uint64, mutex *sync.Mutex) {
		found := v.cachedBitMap.Contains(vIndex)
		if found {
			enconded[vIndex] = nil
			return
		}
		x, _ := v.config.VectorForIDThunk(context.Background(), vIndex)
		enconded[vIndex] = v.pq.Encode(x)
	})
	return enconded
}

func elementsFromMap(set map[uint64]struct{}) []uint64 {
	res := make([]uint64, len(set))
	i := 0
	for x := range set {
		res[i] = x
		i++
	}
	return res
}

func (v *Vamana) robustPrune(p uint64, visited []uint64) {
	visitedSet := NewSet2()
	visitedSet.AddRange(visited).AddRange(v.edges[p]).Remove(p)
	qP, err := v.config.VectorForIDThunk(context.Background(), p)
	if err != nil {
		panic(err)
	}
	out := ssdhelpers.NewFullBitSet(int(v.config.VectorsSize))
	for visitedSet.Size() > 0 {
		pMin := v.closest(qP, visitedSet)
		out.Add(pMin.index)
		qPMin, err := v.config.VectorForIDThunk(context.Background(), pMin.index)
		if err != nil {
			panic(errors.Wrap(err, fmt.Sprintf("Could not fetch vector with id %d", pMin.index)))
		}
		if out.Size() == v.config.R {
			break
		}

		for _, x := range visitedSet.items {
			qX, err := v.config.VectorForIDThunk(context.Background(), x.index)
			if err != nil {
				panic(errors.Wrap(err, fmt.Sprintf("Could not fetch vector with id %d", x.index)))
			}
			if (v.config.Alpha * v.config.Distance(qPMin, qX)) <= x.distance {
				visitedSet.Remove(x.index)
			}
		}
	}
	v.edges[p] = out.Elements()
}

func (v *Vamana) closest(x []float32, set *Set2) *IndexAndDistance {
	var min float32 = math.MaxFloat32
	var indice *IndexAndDistance = nil
	for _, element := range set.items {
		distance := element.distance
		if distance == 0 {
			qi, err := v.config.VectorForIDThunk(context.Background(), element.index)
			if err != nil {
				panic(errors.Wrap(err, fmt.Sprintf("Could not fetch vector with id %d", element.index)))
			}
			distance = v.config.Distance(qi, x)
			element.distance = distance
		}
		if min > distance {
			min = distance
			indice = element
		}
	}
	return indice
}