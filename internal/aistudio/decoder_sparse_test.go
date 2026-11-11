package aistudio

import (
	"bytes"
	"io"
	"math/rand"
	"strings"
	"testing"
	"unicode"
)

// sparseJSONReader 去掉逐字节堆分配（与上游 Mag1cFall/AIStudio2API 4427292 相同）的测试：输出与原先逐字节相同，读取大块稀疏流时
// 分配次数与输入长度无关

// referenceSparseJSON 为原先的实现（每个字节返回一个新切片）：逐字节补齐数组空位，作为对照
func referenceSparseJSON(input []byte) []byte {
	var output []byte
	inString, escaped := false, false
	var previous byte
	for _, value := range input {
		if inString {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == '"' {
				inString = false
			}
			output = append(output, value)
			continue
		}
		switch {
		case value == '"':
			inString = true
			previous = value
			output = append(output, value)
			continue
		case value == ',' && (previous == '[' || previous == ','):
			previous = value
			output = append(output, "null,"...)
			continue
		case value == ']' && previous == ',':
			previous = value
			output = append(output, "null]"...)
			continue
		}
		if !unicode.IsSpace(rune(value)) {
			previous = value
		}
		output = append(output, value)
	}
	return output
}

// readSparseInChunks 经 sparseJSONReader 读完 input，每次 Read 的缓冲区为 size 字节；source 每次只交出 step 字节（模拟网络分块到达）
func readSparseInChunks(t *testing.T, input []byte, size int, step int) []byte {
	t.Helper()
	reader := newSparseJSONReader(&trickleReader{data: input, step: step})
	buffer := make([]byte, size)
	var output []byte
	for {
		count, err := reader.Read(buffer)
		output = append(output, buffer[:count]...)
		if err == io.EOF {
			return output
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// trickleReader 每次最多交出 step 字节
type trickleReader struct {
	data []byte
	step int
}

func (reader *trickleReader) Read(buffer []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, io.EOF
	}
	count := copy(buffer[:min(len(buffer), reader.step)], reader.data)
	reader.data = reader.data[count:]
	return count, nil
}

// TestSparseJSONReaderMatchesReference 稀疏数组（开头、中间、末尾的空位，连续空位，嵌套）、字符串里的逗号与括号、转义的引号与反斜杠、
// 空白与多字节字符，以及随机字节序列：各种读取缓冲区大小（含 1 字节，空位补出的 "null," 要跨多次读取输出）与分块到达方式下，
// 输出都与原先的实现逐字节相同
func TestSparseJSONReaderMatchesReference(t *testing.T) {
	inputs := []string{
		`[,1]`, `[1,,2]`, `[1,]`, `[,,]`, `[[,],[,[,,]]]`, `[ , 1 ,\n, ]`, `[",,]","[,",,"\"",,"\\",]`,
		`[[[[[[null,"hi"]],"model"]]],[[[null,1]]]]`, `["中文，［,］",,"éé",]`, `{"a":[,1],"b":[1,]}`, ``, `[`, `,`,
	}
	random := rand.New(rand.NewSource(1))
	alphabet := []byte(`[],"\ 1a{}:` + "\n")
	for range 300 {
		value := make([]byte, random.Intn(200))
		for index := range value {
			value[index] = alphabet[random.Intn(len(alphabet))]
		}
		inputs = append(inputs, string(value))
	}
	for _, input := range inputs {
		want := referenceSparseJSON([]byte(input))
		for _, size := range []int{1, 2, 3, 4, 5, 7, 64, 4096} {
			for _, step := range []int{1, 3, 1 << 20} {
				if got := readSparseInChunks(t, []byte(input), size, step); !bytes.Equal(got, want) {
					t.Fatalf("输入 %q（缓冲区 %d、分块 %d）：输出 %q，期望 %q", input, size, step, got, want)
				}
			}
		}
	}
}

// sparseTestStream 返回约 size 字节的稀疏流：Playground 帧里带一大段 Base64 内联数据与稀疏数组
func sparseTestStream(size int) []byte {
	frame := `[[[[[[null,null,["image/png","` + strings.Repeat("QUJD", 256) + `"]],[null,"text, with [brackets]",,,]],"model"]],,[1,,2]],`
	var stream bytes.Buffer
	stream.WriteString("[[")
	for stream.Len() < size {
		stream.WriteString(frame)
	}
	stream.WriteString("[[[null,1]]]]]")
	return stream.Bytes()
}

// TestSparseJSONReaderAllocations 读完一段 1 MB 的稀疏流，分配次数与输入长度无关（原先每个字节一次分配，约一百万次）
func TestSparseJSONReaderAllocations(t *testing.T) {
	stream := sparseTestStream(1 << 20)
	buffer := make([]byte, 32<<10)
	source := bytes.NewReader(stream)
	allocations := testing.AllocsPerRun(5, func() {
		source.Reset(stream)
		reader := newSparseJSONReader(source)
		for {
			if _, err := reader.Read(buffer); err != nil {
				return
			}
		}
	})
	if allocations > 64 {
		t.Fatalf("读完 %d 字节的稀疏流分配了 %.0f 次，期望与输入长度无关（不超过 64 次）", len(stream), allocations)
	}
}

// BenchmarkSparseJSONReader 稀疏流解码的读取开销（2 MB 稀疏流）
func BenchmarkSparseJSONReader(b *testing.B) {
	stream := sparseTestStream(2 << 20)
	buffer := make([]byte, 32<<10)
	source := bytes.NewReader(stream)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	for b.Loop() {
		source.Reset(stream)
		reader := newSparseJSONReader(source)
		for {
			if _, err := reader.Read(buffer); err != nil {
				break
			}
		}
	}
}
