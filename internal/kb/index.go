package kb

import (
	"math"
	"sort"
)

// BM25 参数。取值是检索领域的常用默认值:
// k1=1.2 让词频很快饱和 (出现 10 次不比出现 3 次强多少), b=0.75 让长片段不因为
// "字数多所以命中多"而占便宜。
const (
	bm25K1 = 1.2
	bm25B  = 0.75
	// headingBoost 是章节标题里命中的加权倍数。
	//
	// 标题命中通常意味着"这一节就是在讲这个", 比正文里偶然出现一次强得多。
	headingBoost = 2.5
)

// posting 是一条倒排记录: 哪个片段里出现了该词元多少次。
type posting struct {
	chunk int
	tf    float64
}

// index 是内存里的倒排索引。
type index struct {
	postings map[string][]posting
	lengths  []float64 // 每个片段的长度 (词元数)
	avgLen   float64
}

func newIndex() *index {
	return &index{postings: map[string][]posting{}}
}

// add 把一个片段加入索引。
//
// 正文与标题分开计数: 标题的词元额外乘以 headingBoost, 这样"这一节在讲磁盘写满"
// 的片段会排在"正文里顺带提了一句磁盘"的片段前面。
func (idx *index) add(chunkID int, heading, text string) {
	length := 0.0
	counting := map[string]float64{}
	for _, token := range Tokenize(text) {
		counting[token] += 1
		length += 1
	}
	for _, token := range Tokenize(heading) {
		counting[token] += headingBoost
		length += 1
	}
	for token, weight := range counting {
		idx.postings[token] = append(idx.postings[token], posting{chunk: chunkID, tf: weight})
	}
	idx.lengths = append(idx.lengths, length)
}

// finish 在全部片段加入后计算平均长度 (BM25 需要)。
func (idx *index) finish() {
	if len(idx.lengths) == 0 {
		idx.avgLen = 1
		return
	}
	total := 0.0
	for _, length := range idx.lengths {
		total += length
	}
	idx.avgLen = total / float64(len(idx.lengths))
	if idx.avgLen <= 0 {
		idx.avgLen = 1
	}
}

// score 计算某个查询对全部片段的 BM25 得分。
func (idx *index) score(terms []string) []float64 {
	weighted := make([]termWeight, 0, len(terms))
	for _, term := range terms {
		weighted = append(weighted, termWeight{term: term, weight: 1})
	}
	return idx.scoreWeighted(weighted)
}

// scoreWeighted 计算带权词元的 BM25 得分。
//
// 权重用来区分"用户原话"与"模型补充的检索词": 后者只该补充召回, 不该改变排序。
func (idx *index) scoreWeighted(terms []termWeight) []float64 {
	scores := make([]float64, len(idx.lengths))
	documentCount := float64(len(idx.lengths))
	if documentCount == 0 {
		return scores
	}

	for _, entry := range terms {
		term, weight := entry.term, entry.weight
		if weight <= 0 {
			weight = 1
		}
		list, ok := idx.postings[term]
		if !ok {
			continue
		}
		// IDF: 出现在越少片段里的词越有区分度
		df := float64(len(list))
		idf := math.Log(1 + (documentCount-df+0.5)/(df+0.5))
		if idf <= 0 {
			// 出现在所有片段里的词 (如"备份") 没有区分度, 但也不能因此让
			// 只有它的查询得零分 —— 给一个极小值
			idf = 0.05
		}
		for _, posting := range list {
			chunkID := posting.chunk
			length := idx.lengths[chunkID]
			if length <= 0 {
				length = 1
			}
			denominator := posting.tf + bm25K1*(1-bm25B+bm25B*length/idx.avgLen)
			if denominator <= 0 {
				continue
			}
			scores[chunkID] += weight * idf * posting.tf * (bm25K1 + 1) / denominator
		}
	}
	return scores
}

// Result 是一条检索结果。
type Result struct {
	ChunkID int     `json:"-"`
	Score   float64 `json:"score"`
	DocID   string  `json:"doc_id"`
	Doc     string  `json:"doc"`
	Heading string  `json:"heading,omitempty"`
	Text    string  `json:"text"`
	// Matched 是命中的词元, 用于解释"为什么这条被检索出来"。
	Matched []string `json:"matched,omitempty"`
}

// rank 返回得分最高的片段 (按分数降序)。
//
// 会丢掉"得分只有最高分一小部分"的结果: 检索总有噪声, 把明显不相关的片段塞进提示词
// 只会让模型分心、让引用变多而变虚。
func (idx *index) rank(scores []float64, terms []string, chunks []Chunk, topK int) []Result {
	if len(scores) == 0 {
		return nil
	}
	ordered := make([]int, 0, len(scores))
	for chunkID, score := range scores {
		if score > 0 {
			ordered = append(ordered, chunkID)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if scores[ordered[i]] != scores[ordered[j]] {
			return scores[ordered[i]] > scores[ordered[j]]
		}
		return ordered[i] < ordered[j] // 同分按文档顺序, 保证结果稳定
	})
	if len(ordered) == 0 {
		return nil
	}
	if topK > 0 && len(ordered) > topK {
		ordered = ordered[:topK]
	}

	best := scores[ordered[0]]
	cutoff := best * 0.15
	out := make([]Result, 0, len(ordered))
	for _, chunkID := range ordered {
		if scores[chunkID] < cutoff {
			continue
		}
		chunk := chunks[chunkID]
		out = append(out, Result{
			ChunkID: chunkID,
			Score:   math.Round(scores[chunkID]*1000) / 1000,
			Heading: chunk.Heading,
			Text:    chunk.Text,
			Matched: matchedTerms(chunk, terms),
		})
	}
	return out
}

// matchedTerms 返回该片段里实际命中的查询词元 (最多 8 个, 避免界面被刷满)。
func matchedTerms(chunk Chunk, terms []string) []string {
	present := map[string]bool{}
	for _, token := range Tokenize(chunk.Text + "\n" + chunk.Heading) {
		present[token] = true
	}
	out := []string{}
	for _, term := range terms {
		if present[term] {
			out = append(out, term)
			if len(out) >= 8 {
				break
			}
		}
	}
	return out
}
