package smoke_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

func inferenceSHA(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// 每层拒绝链接，再在Root内复核打开的目录身份；Root阻止并发替换逃逸到树外。
func inferenceOpenDir(path string, create bool) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("目录必须为规范绝对路径")
	}
	r, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, errors.New("无法锚定目录")
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		info, e := r.Lstat(part)
		if os.IsNotExist(e) && create {
			e = r.Mkdir(part, 0700)
			if e == nil || os.IsExist(e) {
				info, e = r.Lstat(part)
			}
		}
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			r.Close()
			return nil, errors.New("目录缺失或含链接")
		}
		next, e := r.OpenRoot(part)
		if e != nil {
			r.Close()
			return nil, errors.New("目录打开失败")
		}
		a, e := next.Stat(".")
		b, e2 := r.Lstat(part)
		if e != nil || e2 != nil || !os.SameFile(info, a) || !os.SameFile(info, b) || !b.IsDir() {
			next.Close()
			r.Close()
			return nil, errors.New("目录身份改变")
		}
		r.Close()
		r = next
	}
	return r, nil
}

func inferenceReadFile(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("文件必须为绝对路径")
	}
	r, err := inferenceOpenDir(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	name := filepath.Base(path)
	info, err := r.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("文件非普通或超限")
	}
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("文件打开失败")
	}
	defer f.Close()
	got, err := f.Stat()
	if err != nil || !got.Mode().IsRegular() || !os.SameFile(info, got) {
		return nil, errors.New("文件身份改变")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("文件读取超限")
	}
	return b, nil
}

func inferenceExclusive(r *os.Root, name string) (*os.File, error) {
	if err := inferencePrivateDir(r); err != nil {
		return nil, err
	}
	if filepath.Base(name) != name {
		return nil, errors.New("非法文件名")
	}
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("独占文件已存在或不能创建")
	}
	return f, nil
}

func inferencePrivateDir(r *os.Root) error {
	i, err := r.Stat(".")
	if err != nil || i.Mode().Perm() != 0700 {
		return errors.New("证据目录必须为0700")
	}
	return nil
}

func inferenceWrite(r *os.Root, name string, b []byte) error {
	f, err := inferenceExclusive(r, name)
	if err != nil {
		return err
	}
	_, we := f.Write(b)
	se := f.Sync()
	ce := f.Close()
	if we != nil || se != nil || ce != nil {
		return errors.New("持久写入失败，槽不退款")
	}
	d, err := r.Open(".")
	if err != nil {
		return errors.New("目录同步失败")
	}
	defer d.Close()
	if d.Sync() != nil {
		return errors.New("目录同步失败")
	}
	return nil
}

// 冻结身份和六槽预算；未占槽的费用材料可补齐，已占槽的具体材料固定在reserve中。
// 六个唯一槽的预算总和已预检；并发不需可回收锁或退款。
func inferenceReservation(c inferenceRecordingConfig) (budget, claim []byte, err error) {
	a, err := inferenceAuthorizeCost(c.Manifest, c.Slot, time.Now())
	if err != nil {
		return nil, nil, err
	}
	m := c.Manifest
	m.CostEvidence = nil
	// token最大量是本槽费用证明的一部分，不是六槽共享的退款额度。
	// 未占槽的0表示未知，后补正数不改固定人民币/task/时长预算；占槽后按完整Slot封存。
	m.Slots[3].WorstCaseTokens = 0
	m.Slots[4].WorstCaseTokens = 0
	budget, _ = json.Marshal(m)
	claim, _ = json.Marshal(struct {
		Slot           inferenceRecordingSlot
		WallSeconds    int64
		ManifestSHA256 string
		Cost           inferenceCostAuthorization
	}{c.Manifest.Slots[c.Slot-1], 45, inferenceSHA(budget), a})
	if len(claim) > inferenceManifestLimit {
		return nil, nil, errors.New("费用占槽记录超限")
	}
	return budget, claim, nil
}

func inferenceReserve(c inferenceRecordingConfig) error {
	if err := inferenceValidate(c); err != nil {
		return err
	}
	b, claim, err := inferenceReservation(c)
	if err != nil {
		return err
	}
	root := filepath.Dir(c.Output)
	r, err := inferenceOpenDir(root, true)
	if err != nil {
		return err
	}
	defer r.Close()
	name := filepath.Base(c.Output)
	if err := inferencePrivateDir(r); err != nil {
		return err
	}
	if _, err := r.Lstat(name); !os.IsNotExist(err) {
		return errors.New("输出必须不存在")
	}
	if _, err := r.Lstat("manifest.json"); os.IsNotExist(err) {
		if err := inferenceWrite(r, "manifest.json", b); err != nil {
			return err
		}
	} else {
		old, err := inferenceReadFile(filepath.Join(root, "manifest.json"), inferenceManifestLimit)
		if err != nil || !bytes.Equal(old, b) {
			return errors.New("批次manifest已冻结或损坏")
		}
	}
	if err := inferenceWrite(r, fmt.Sprintf("reserve-%d.json", c.Slot), claim); err != nil {
		return err
	}
	if err := r.Mkdir(name, 0700); err != nil {
		return errors.New("创建输出失败，槽仍消耗")
	}
	d, err := r.Open(".")
	if err != nil {
		return errors.New("输出目录同步失败")
	}
	defer d.Close()
	return d.Sync()
}

func inferenceClaimDial(c inferenceRecordingConfig) error {
	if err := inferenceValidate(c); err != nil {
		return err
	}
	b, expected, err := inferenceReservation(c)
	if err != nil {
		return err
	}
	r, err := inferenceOpenDir(filepath.Dir(c.Output), false)
	if err != nil {
		return err
	}
	defer r.Close()
	old, err := inferenceReadFile(filepath.Join(filepath.Dir(c.Output), "manifest.json"), inferenceManifestLimit)
	if err != nil || !bytes.Equal(b, old) {
		return errors.New("拨号前manifest不符")
	}
	claim, err := inferenceReadFile(filepath.Join(filepath.Dir(c.Output), fmt.Sprintf("reserve-%d.json", c.Slot)), inferenceManifestLimit)
	if err != nil || !bytes.Equal(claim, expected) {
		return errors.New("拨号缺持久reserve")
	}
	return inferenceWrite(r, fmt.Sprintf("dial-%d", c.Slot), []byte("spent\n"))
}

// token扫描先于树解码，拒绝转义重复键、秘密与深度放大，不依赖被测Inspector。
func inferenceJSON(b []byte, key string) (any, error) {
	return inferenceScanJSON(b, key, true)
}

// raw安全扫描允许重复键但仍遍历每个值查秘密；严格scenario解码另外拒绝重复。
func inferenceScanJSON(b []byte, key string, strict bool) (any, error) {
	bad := errors.New("JSON重复键、秘密或结构非法")
	if len(b) > inferenceMessageLimit || !utf8.Valid(b) || !json.Valid(b) || key != "" && bytes.Contains(b, []byte(key)) {
		return nil, bad
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var scan func(int) (any, error)
	scan = func(depth int) (any, error) {
		if depth > 32 {
			return nil, bad
		}
		tok, err := d.Token()
		if err != nil {
			return nil, bad
		}
		switch v := tok.(type) {
		case string:
			if key != "" && strings.Contains(v, key) {
				return nil, bad
			}
			return v, nil
		case json.Delim:
			if v == '{' {
				m := map[string]any{}
				seen := map[string]bool{}
				for d.More() {
					tok, e := d.Token()
					k, ok := tok.(string)
					if e != nil || !ok {
						return nil, bad
					}
					fold := strings.ToLower(k)
					if strict && seen[fold] || key != "" && strings.Contains(k, key) {
						return nil, bad
					}
					seen[fold] = true
					switch strings.ReplaceAll(fold, "-", "_") {
					case "authorization", "api_key", "apikey", "cookie", "set_cookie", "access_token", "secret", "client_secret":
						return nil, bad
					}
					value, e := scan(depth + 1)
					if e != nil {
						return nil, e
					}
					m[k] = value
				}
				if _, e := d.Token(); e != nil {
					return nil, bad
				}
				return m, nil
			}
			if v == '[' {
				a := []any{}
				for d.More() {
					x, e := scan(depth + 1)
					if e != nil {
						return nil, e
					}
					a = append(a, x)
				}
				if _, e := d.Token(); e != nil {
					return nil, bad
				}
				return a, nil
			}
			return nil, bad
		default:
			return tok, nil
		}
	}
	return scan(0)
}
