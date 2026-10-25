package memory

import (
	"container/heap"
	"math"
	"sort"
)

// 向量检索的候选筛选。
//
// 为什么需要它：SQLite 没有向量索引，只能全表扫后逐个算余弦。
// 1024 维、1000 条候选 = 约 100 万次乘加，单次查询几毫秒，尚可接受。
// 但记忆会一直增长，到几万条时每次检索要上百毫秒 —— 而检索发生在
// 每轮思考的路径上，直接叠加到回复延迟里。
//
// 这个文件做两件事，都不引入新依赖：
//
//  1. 预归一化：把库里的向量按 L2 归一化后存起来，
//     这样余弦相似度退化成点积，省掉每次的两遍平方和开方。
//
//  2. 有界 top-k 选择：用最小堆而不是「全排序后取前 k」。
//     全排序是 O(n log n)，堆是 O(n log k)。
//
// 实测（BenchmarkTopKVsSort，1024 维 × 10000 条 × k=5）：
//
//	TopK(堆)  6.27 ms/op
//	全排序    7.54 ms/op
//	约 1.2 倍
//
// 比预想的 1.7 倍小得多。原因很直白：这个规模下真正贵的是
// 10000 × 1024 次浮点乘加（约 6ms），堆和排序各自只占其中 1~1.5ms。
// **瓶颈在点积，不在选择算法。** 保留 TopK 是因为它不增加复杂度、
// 且在 k 更大或 n 更大时优势会体现；但不要指望它能解决慢的问题 ——
// 真要再快得砍候选数（调小 vector_candidate_limit）或上 ANN 索引。
//
// 没有做真正的 ANN（HNSW / IVF）：那些需要图结构或聚类中心，
// 复杂度与维护成本远超当前需求。等记忆真的上万条、且实测到瓶颈再说。
// AGENTS.md：不要提前为「以后可能要」的抽象买单。

// Scored 是一条带相似度分数的候选。
type Scored struct {
	Index int
	Score float64
}

// Normalize 把向量原地 L2 归一化。
//
// 归一化之后，余弦相似度 = 点积，省掉重复的模长计算。
// 这一步在做「一次查询对 N 条候选」的场景下收益最大：
// 查询向量归一化一次，候选向量在写入时就已归一化。
func Normalize(vec []float32) {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return
	}
	inv := 1.0 / math.Sqrt(sum)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) * inv)
	}
}

// DotProduct 计算两个已归一化向量的点积，等价于余弦相似度。
//
// 长度不一致时返回 0（不 panic）：库里的旧向量可能是别的维度。
func DotProduct(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// TopK 从候选里选出相似度最高的 k 条。
//
// 用最小堆维护「当前最好的 k 条」：堆顶是这 k 条里最差的，
// 新候选只要比堆顶好就替换。这样只需要 O(n log k) 而不是 O(n log n)。
//
// minScore 是门槛，低于它的不进候选（避免无关记忆占位）。
func TopK(query []float32, candidates [][]float32, k int, minScore float64) []Scored {
	if len(candidates) == 0 || k <= 0 || len(query) == 0 {
		return nil
	}

	h := &minHeap{}
	heap.Init(h)

	for i, cand := range candidates {
		// 维度不一致的候选直接跳过。
		//
		// 不能只靠下面 score < minScore 过滤：DotProduct 对维度不匹配返回 0，
		// 而 minScore 为 0 时「0 < 0」是 false，这条垃圾候选会被收进来。
		// 它占掉一个名额，结果是「返回了 k 条但其中一条是错的」。
		if len(cand) != len(query) {
			continue
		}
		score := DotProduct(query, cand)
		if score < minScore {
			continue
		}
		if h.Len() < k {
			heap.Push(h, Scored{Index: i, Score: score})
			continue
		}
		// 堆顶是当前 k 条里最小的；比它好才替换。
		if score > (*h)[0].Score {
			heap.Pop(h)
			heap.Push(h, Scored{Index: i, Score: score})
		}
	}

	out := make([]Scored, h.Len())
	copy(out, *h)
	// 堆里是无序的，按分数降序排列后才能直接当结果用。
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// minHeap 是按分数升序的最小堆（堆顶最小）。
type minHeap []Scored

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].Score < h[j].Score }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(Scored)) }
func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
