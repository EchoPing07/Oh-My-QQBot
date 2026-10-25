package memory

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

// TestNormalizeProducesUnitLength 验证归一化后的模长为 1。
func TestNormalizeProducesUnitLength(t *testing.T) {
	v := []float32{3, 4} // 模长 5
	Normalize(v)

	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(sum)-1) > 1e-5 {
		t.Errorf("模长 = %v，期望 1", math.Sqrt(sum))
	}
	// 3/5, 4/5
	if math.Abs(float64(v[0])-0.6) > 1e-5 || math.Abs(float64(v[1])-0.8) > 1e-5 {
		t.Errorf("归一化结果 = %v，期望 [0.6 0.8]", v)
	}
}

// TestNormalizeZeroVector 验证零向量不产生 NaN。
//
// 零向量除以 0 会得到 NaN，而 NaN 参与比较永远为 false，
// 表现是「这条记忆永远不进候选」但不报错。
func TestNormalizeZeroVector(t *testing.T) {
	v := []float32{0, 0, 0}
	Normalize(v)

	for _, x := range v {
		if math.IsNaN(float64(x)) {
			t.Error("零向量归一化产生了 NaN")
		}
	}
}

// TestDotProductEqualsCosine 验证归一化后点积等于余弦。
func TestDotProductEqualsCosine(t *testing.T) {
	a := []float32{1, 2, 3, 4}
	b := []float32{2, 1, 0, 3}

	// 先算原始余弦。
	want := cosineReference(a, b)

	// 归一化后算点积。
	an := append([]float32(nil), a...)
	bn := append([]float32(nil), b...)
	Normalize(an)
	Normalize(bn)
	got := DotProduct(an, bn)

	if math.Abs(got-want) > 1e-5 {
		t.Errorf("点积 = %v，余弦 = %v，应该相等", got, want)
	}
}

// TestDotProductLengthMismatch 验证维度不一致返回 0 而不是 panic。
func TestDotProductLengthMismatch(t *testing.T) {
	if got := DotProduct([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Errorf("维度不一致应返回 0，实际 %v", got)
	}
}

// TestTopKSelectsHighest 验证选出的是分数最高的 k 条。
func TestTopKSelectsHighest(t *testing.T) {
	query := []float32{1, 0, 0}

	// 三条候选，与 query 的相似度分别是 1.0 / 0.707 / 0.0
	c0 := []float32{1, 0, 0}
	c1 := []float32{1, 1, 0}
	c2 := []float32{0, 1, 0}
	for _, c := range [][]float32{c0, c1, c2} {
		Normalize(c)
	}
	Normalize(query)

	got := TopK(query, [][]float32{c0, c1, c2}, 2, 0)

	if len(got) != 2 {
		t.Fatalf("返回 %d 条，期望 2", len(got))
	}
	// 分数应降序。
	if got[0].Score < got[1].Score {
		t.Error("结果应按分数降序")
	}
	// 最高分是 c0（相似度 1）。
	if got[0].Index != 0 {
		t.Errorf("首条 index = %d，期望 0", got[0].Index)
	}
	// 次高分是 c1。
	if got[1].Index != 1 {
		t.Errorf("次条 index = %d，期望 1", got[1].Index)
	}
}

// TestTopKRespectsMinScore 验证低于门槛的被过滤。
//
// 没有门槛的话，问一句完全无关的话也会返回「最不无关」的那条，
// 把无意义内容塞进上下文。
func TestTopKRespectsMinScore(t *testing.T) {
	query := []float32{1, 0}
	Normalize(query)

	// 与 query 正交，相似度 0。
	c := []float32{0, 1}
	Normalize(c)

	got := TopK(query, [][]float32{c}, 5, 0.3)
	if len(got) != 0 {
		t.Errorf("低于门槛不该入选，实际返回 %d 条", len(got))
	}
}

// TestTopKEmptyInputs 验证各种空输入不 panic。
func TestTopKEmptyInputs(t *testing.T) {
	if got := TopK(nil, nil, 5, 0); got != nil {
		t.Error("空输入应返回 nil")
	}
	if got := TopK([]float32{1}, nil, 5, 0); got != nil {
		t.Error("空候选应返回 nil")
	}
	if got := TopK([]float32{1}, [][]float32{{1}}, 0, 0); got != nil {
		t.Error("k=0 应返回 nil")
	}
}

// TestTopKHandlesShortCandidates 验证维度不匹配的候选被跳过。
//
// 库里的旧向量可能是别的维度，不能让整次检索失败。
func TestTopKHandlesShortCandidates(t *testing.T) {
	query := []float32{1, 0, 0}
	Normalize(query)

	good := []float32{1, 0, 0}
	Normalize(good)

	got := TopK(query, [][]float32{{1, 0}, good}, 5, 0)
	if len(got) != 1 {
		t.Fatalf("应只返回 1 条（另一个维度不匹配），实际 %d", len(got))
	}
	if got[0].Index != 1 {
		t.Errorf("应选中 index 1，实际 %d", got[0].Index)
	}
}

// TestTopKMatchesFullSort 用随机数据对照「全排序取前 k」。
//
// 这个测试的价值：TopK 用堆实现，容易写出边界错误
// （k 刚好等于候选数、有重复分数、minScore 卡在边界）。
// 用随机数据对拍能覆盖这些边角。
func TestTopKMatchesFullSort(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	for trial := 0; trial < 20; trial++ {
		n := 1 + rng.Intn(50)
		dim := 1 + rng.Intn(20)
		k := 1 + rng.Intn(n)

		query := make([]float32, dim)
		for i := range query {
			query[i] = rng.Float32()*2 - 1
		}
		Normalize(query)

		cands := make([][]float32, n)
		for i := range cands {
			v := make([]float32, dim)
			for j := range v {
				v[j] = rng.Float32()*2 - 1
			}
			Normalize(v)
			cands[i] = v
		}

		// 参考实现：全排序。
		type pair struct {
			idx   int
			score float64
		}
		all := make([]pair, n)
		for i, c := range cands {
			all[i] = pair{i, DotProduct(query, c)}
		}
		// 降序。
		for i := 1; i < len(all); i++ {
			for j := i; j > 0 && all[j].score > all[j-1].score; j-- {
				all[j], all[j-1] = all[j-1], all[j]
			}
		}
		wantN := k
		if wantN > len(all) {
			wantN = len(all)
		}
		want := all[:wantN]

		// minScore 用 -2（比任何可能出现的余弦都低），等价于「不设门槛」。
		//
		// 不能用 0：随机数据的余弦有正有负，用 0 会把负分候选过滤掉，
		// 而参考实现（全排序）没有这个过滤，两者不可比。
		got := TopK(query, cands, k, -2)

		if len(got) != len(want) {
			t.Fatalf("trial %d: 返回 %d 条，期望 %d", trial, len(got), len(want))
		}
		for i := range got {
			if math.Abs(got[i].Score-want[i].score) > 1e-5 {
				t.Errorf("trial %d 第 %d 条分数 = %v，期望 %v",
					trial, i, got[i].Score, want[i].score)
			}
		}
	}
}

// cosineReference 是独立实现的余弦，用于对拍。
func cosineReference(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// BenchmarkTopKVsSort 对比堆选择与全排序。
//
// 用途：记忆上万条时，这个差别会直接体现在回复延迟上。
// 跑法：go test ./internal/memory/ -bench BenchmarkTopKVsSort -run '^$'
func BenchmarkTopKVsSort(b *testing.B) {
	const (
		n   = 10000
		dim = 1024
		k   = 5
	)
	r := rand.New(rand.NewSource(1))

	query := make([]float32, dim)
	for i := range query {
		query[i] = r.Float32()
	}
	Normalize(query)

	cands := make([][]float32, n)
	for i := range cands {
		v := make([]float32, dim)
		for j := range v {
			v[j] = r.Float32()
		}
		Normalize(v)
		cands[i] = v
	}

	b.Run("TopK(堆)", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			TopK(query, cands, k, 0)
		}
	})

	b.Run("全排序", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			scores := make([]Scored, n)
			for j, c := range cands {
				scores[j] = Scored{Index: j, Score: DotProduct(query, c)}
			}
			// 简易插入排序会太慢，用标准库排序模拟「全排序」。
			sortScoredDesc(scores)
			_ = scores[:k]
		}
	})
}

// sortScoredDesc 按分数降序排序，用于基准对照。
func sortScoredDesc(s []Scored) {
	sort.Slice(s, func(i, j int) bool { return s[i].Score > s[j].Score })
}
