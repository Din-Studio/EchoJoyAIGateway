package control

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// 核对后端 operation 枚举与前端日志筛选契约一致，不执行前端或浏览器测试。
func TestSharedAPIOperationsMatchUIContract(t *testing.T) {
	t.Parallel()
	content := readUIContractFile(t, filepath.Join("internal", "execution", "contracts.go"))
	operations := regexp.MustCompile(`Operation\w+\s+Operation\s*=\s*"([^"]+)"`).FindAllStringSubmatch(content, -1)
	if len(operations) == 0 {
		t.Fatal("execution operation contract is empty")
	}
	for _, test := range []struct {
		path  string
		array string
	}{
		{"frontends/modern/api/logs.ts", "logOperations"},
	} {
		t.Run(test.path, func(t *testing.T) {
			allowed := readUIContractArray(t, readUIContractFile(t, filepath.Join("web", "src", test.path)), test.array)
			for _, operation := range operations {
				if !allowed[operation[1]] {
					t.Errorf("shared API operation %q is rejected by %s", operation[1], test.path)
				}
			}
		})
	}
}

func readUIContractFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func readUIContractArray(t *testing.T, content, name string) map[string]bool {
	t.Helper()
	pattern := `(?s)\bconst\s+` + regexp.QuoteMeta(name) + `\s*=\s*\[(.*?)\]\s*as const`
	match := regexp.MustCompile(pattern).FindStringSubmatch(content)
	if match == nil {
		t.Fatalf("UI contract array %q not found", name)
	}
	values := make(map[string]bool)
	for _, value := range regexp.MustCompile(`'([^']+)'|\.\.\.(\w+)`).FindAllStringSubmatch(match[1], -1) {
		if value[1] != "" {
			values[value[1]] = true
			continue
		}
		for field := range readUIContractArray(t, content, value[2]) {
			values[field] = true
		}
	}
	return values
}
