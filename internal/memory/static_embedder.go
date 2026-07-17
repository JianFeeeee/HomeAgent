package memory

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yanyiwu/gojieba"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

const downloadMaxWords = 200000

var knownModelURLs = []struct {
	sub string
	url string
}{
	{"numberbatch", "https://conceptnet.s3.amazonaws.com/downloads/2019/numberbatch/numberbatch-19.08.txt.gz"},
	{"cc.zh.", "https://dl.fbaipublicfiles.com/fasttext/vectors-crawl/cc.zh.300.vec.gz"},
	{"cc.en.", "https://dl.fbaipublicfiles.com/fasttext/vectors-crawl/cc.en.300.vec.gz"},
}

type StaticEmbedder struct {
	mu        sync.RWMutex
	jieba     *gojieba.Jieba
	stopWords map[string]bool

	words  map[string][]float64
	dim    int
	loaded bool

	unkVec  []float64
	unkNorm float64
}

func modelDownloadURL(modelPath string) string {
	for _, m := range knownModelURLs {
		if strings.Contains(modelPath, m.sub) {
			return m.url
		}
	}
	return knownModelURLs[0].url
}

func downloadFastTextModel(targetPath, url string) error {
	tmpPath := targetPath + ".download.tmp"
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	defer f.Close()

	resp, err := http.Get(url)
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		os.Remove(tmpPath)
		return fmt.Errorf("http status %s", resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	scanner := bufio.NewScanner(gz)
	buf := make([]byte, 4*1024*1024)
	scanner.Buffer(buf, len(buf))

	writer := bufio.NewWriter(f)

	if !scanner.Scan() {
		os.Remove(tmpPath)
		return fmt.Errorf("empty gzip content")
	}
	parts := strings.Fields(scanner.Text())
	if len(parts) >= 2 {
		fmt.Fprintf(writer, "%d %s\n", downloadMaxWords, parts[1])
	} else {
		fmt.Fprintln(writer, scanner.Text())
	}

	var lineCount int
	for scanner.Scan() && lineCount < downloadMaxWords {
		line := scanner.Text()
		if line == "" {
			continue
		}
		fmt.Fprintln(writer, line)
		lineCount++

		if lineCount%50000 == 0 {
			log.Printf("[static_embedder] download progress: %d/%d words", lineCount, downloadMaxWords)
		}
	}

	writer.Flush()
	f.Close()

	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	log.Printf("[static_embedder] download complete: %d words to %s", lineCount, targetPath)
	return nil
}

func ensureModelFile(modelPath string) {
	if modelPath == "" {
		return
	}
	if _, err := os.Stat(modelPath); err == nil {
		return
	}
	url := modelDownloadURL(modelPath)
	log.Printf("[static_embedder] model %s not found, downloading from fastText...", modelPath)
	if dlErr := downloadFastTextModel(modelPath, url); dlErr != nil {
		log.Printf("[static_embedder] download failed: %v, will use TF-IDF fallback", dlErr)
	} else {
		log.Printf("[static_embedder] download ok")
	}
}

func NewStaticEmbedder(modelPaths ...string) *StaticEmbedder {
	sw := make(map[string]bool)
	for k, v := range stopWords {
		sw[k] = v
	}
	e := &StaticEmbedder{
		jieba:     GetJieba(),
		stopWords: sw,
		words:     make(map[string][]float64),
	}

	if len(modelPaths) == 0 {
		log.Printf("[static_embedder] no model path configured, using TF-IDF fallback")
		return e
	}

	for _, p := range modelPaths {
		ensureModelFile(p)
	}
	if err := e.loadAll(modelPaths); err != nil {
		log.Printf("[static_embedder] load failed: %v, using TF-IDF fallback", err)
	}
	return e
}

func (e *StaticEmbedder) loadAll(paths []string) error {
	var firstErr error
	for i, p := range paths {
		if p == "" {
			continue
		}
		primary := i == 0
		if err := e.load(p, primary); err != nil {
			log.Printf("[static_embedder] load %s: %v", p, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (e *StaticEmbedder) load(path string, primary bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, len(buf))

	if !scanner.Scan() {
		return fmt.Errorf("empty file")
	}
	header := strings.TrimSpace(scanner.Text())
	parts := strings.Fields(header)
	if len(parts) < 2 {
		return fmt.Errorf("invalid header: %s", header)
	}
	dim, err := strconv.Atoi(parts[1])
	if err != nil || dim <= 0 {
		return fmt.Errorf("invalid dimension: %s", parts[1])
	}

	if primary {
		e.dim = dim
	}

	var vecSum []float64
	var count int
	if primary {
		vecSum = make([]float64, dim)
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < dim+1 {
			continue
		}
		word := fields[0]

		if _, exists := e.words[word]; exists {
			continue
		}

		vec := make([]float64, dim)
		for i := 0; i < dim; i++ {
			v, _ := strconv.ParseFloat(fields[i+1], 64)
			vec[i] = v
		}
		e.words[word] = vec
		if primary {
			for i := range vecSum {
				vecSum[i] += vec[i]
			}
			count++
		}
	}

	if primary {
		if count == 0 {
			return fmt.Errorf("no word vectors found in primary model")
		}
		for i := range vecSum {
			vecSum[i] /= float64(count)
		}
		e.unkVec = make([]float64, dim)
		copy(e.unkVec, vecSum)
		var normSq float64
		for _, v := range e.unkVec {
			normSq += v * v
		}
		e.unkNorm = float64(math.Sqrt(normSq))
		e.loaded = true
	}

	log.Printf("[static_embedder] loaded %d words, dim=%d from %s", len(e.words), e.dim, path)
	return nil
}

func (e *StaticEmbedder) tokenize(text string) []string {
	if e.jieba == nil {
		return nil
	}
	words := e.jieba.Cut(text, true)
	var result []string
	seen := make(map[string]bool)
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" || e.stopWords[w] || seen[w] {
			continue
		}
		if utf8.RuneCountInString(w) < 2 {
			continue
		}
		seen[w] = true
		result = append(result, w)
	}
	return result
}

func (e *StaticEmbedder) Vectorize(text string) vector.Vector {
	e.mu.RLock()
	loaded := e.loaded
	dim := e.dim
	unkVec := e.unkVec
	e.mu.RUnlock()

	tokens := e.tokenize(text)
	if len(tokens) == 0 {
		return vector.Vector{}
	}

	tf := make(map[string]float64)
	for _, t := range tokens {
		tf[t]++
	}
	maxTF := 0.0
	for _, c := range tf {
		if c > maxTF {
			maxTF = c
		}
	}

	if !loaded {
		vec := make(vector.Vector)
		for word, count := range tf {
			vec[word] = count / maxTF
		}
		return vec
	}

	sum := make([]float64, dim)
	var weightSum float64

	for word, count := range tf {
		e.mu.RLock()
		vec, ok := e.words[word]
		e.mu.RUnlock()

		w := count / maxTF

		if !ok {
			for i, v := range unkVec {
				sum[i] += w * v
			}
		} else {
			for i, v := range vec {
				sum[i] += w * v
			}
		}
		weightSum += w
	}

	if weightSum > 0 {
		for i := range sum {
			sum[i] /= weightSum
		}
	}

	vec := make(vector.Vector, dim)
	for i, v := range sum {
		if v != 0 {
			vec[strconv.Itoa(i)] = v
		}
	}
	return vec
}

func (e *StaticEmbedder) Dim() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dim
}

func (e *StaticEmbedder) Loaded() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.loaded
}
