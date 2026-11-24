package rmtes

import (
	"bytes"
	"testing"
)

// 全部非 ASCII 期望值均可从 C 表逐项核对：
//
//	tblRBCS2[0x00]            = 0x00A1  -> C2 A1
//	tblCNS1a[0x000]           = 0x3000  -> E3 80 80
//	tbl208b[1410-table2Start] = 0x4E9C  -> E4 BA 9C
//	tblKatakana[0]            = 0xFF61  -> EF BD A1
//
// 生成方式见 gen_tables.py（来源 rsslcnvtab.c，commit 7d219a600，Apache-2.0）。

func TestToUTF8PureASCII(t *testing.T) {
	in := []byte("ABC xyz 123")
	got, err := ToUTF8(in)
	if err != nil {
		t.Fatalf("ToUTF8(%q) error: %v", in, err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("ToUTF8(%q) = % x, want % x", in, got, in)
	}
}

func TestToUTF8SpacePadding(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"环绕空格", []byte("  PAD  "), []byte("  PAD  ")},
		{"制表前导空格", []byte("  A"), []byte("  A")},
		{"94 集 DEL 映射 U+FFFD", []byte{'A', 0x7F, 'B'}, []byte{'A', 0xEF, 0xBF, 0xBD, 'B'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ToUTF8(tc.in)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got % x, want % x", got, tc.want)
			}
		})
	}
}

func TestToUTF8ControlChars(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		// 0x1C = END_CHAR：到此截断，后续字母不再输出。
		{"结束符截断", []byte{'A', 'B', 0x1C, 'C', 'D'}, []byte("AB")},
		// ESC 25 30 切换到 UTF-8 编码：其后字节原样拷贝。
		{"UTF-8 转义直通", []byte{0x1B, 0x25, 0x30, 0xC3, 0xA9}, []byte{0xC3, 0xA9}},
		// ESC ( I 将 G0 切到片假名集，0x21 -> tblKatakana[0]=0xFF61 -> EF BD A1。
		{"转义切换字符集", []byte{0x1B, 0x28, 0x49, 0x21}, []byte{0xEF, 0xBD, 0xA1}},
		// 0x9B(CSI) 落在 CR 区间且非 SS2/SS3 -> U+FFFD。
		{"CR 保留字节替换", []byte{0x9B}, []byte{0xEF, 0xBF, 0xBD}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ToUTF8(tc.in)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got % x, want % x", got, tc.want)
			}
		})
	}
}

func TestToUTF8TableNonASCII(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		// 默认 GR=ReuterBasic2，0xA1 -> rbcs2[0]=0x00A1 (¡)。
		{"GR 表单字节", []byte{0xA1}, []byte{0xC2, 0xA1}},
		// ESC $ ( G 把 G0 切到中文集1（stride 2）；0x21 0x21 -> cns1a[0]=0x3000。
		{"GL stride2 表1", []byte{0x1B, 0x24, 0x28, 0x47, 0x21, 0x21}, []byte{0xE3, 0x80, 0x80}},
		// ESC & @ ESC $ B 把 G0 切到日文汉字（stride 2, table2）；0x30 0x21 -> idx1410=208b[0]=0x4E9C。
		{"GL stride2 表2", []byte{0x1B, 0x26, 0x40, 0x1B, 0x24, 0x42, 0x30, 0x21}, []byte{0xE4, 0xBA, 0x9C}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ToUTF8(tc.in)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got % x, want % x", got, tc.want)
			}
		})
	}
}

func TestToUTF8InvalidSequence(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"ESC ! 后必须是 0x40", []byte{0x1B, 0x21, 0x41}},
		{"ESC [ 数字截断无终结符", []byte{0x1B, 0x5B, 0x33}},
		{"超出 94 集 GR 范围", []byte{0xFF}},
		{"stride2 缺第二个字节", []byte{0x1B, 0x24, 0x28, 0x47, 0x21}},
		{"SS2 后接非法 GL", []byte{0x8E, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ToUTF8(tc.in)
			if err == nil {
				t.Fatalf("ToUTF8(% x) = % x, want fail-closed error", tc.in, got)
			}
			if got != nil {
				t.Fatalf("ToUTF8(% x) 出错时仍返回数据 % x", tc.in, got)
			}
		})
	}
}

func TestToUTF8EmptyInput(t *testing.T) {
	for _, in := range [][]byte{nil, {}} {
		got, err := ToUTF8(in)
		if err != nil {
			t.Fatalf("ToUTF8(% x) error: %v", in, err)
		}
		if len(got) != 0 {
			t.Fatalf("ToUTF8(% x) = % x, want empty", in, got)
		}
	}
}

func TestToUTF8RepeatCommand(t *testing.T) {
	// ESC [ 3 b：重复前一字符 'A' 三次。
	in := []byte{'A', 0x1B, 0x5B, 0x33, 0x62}
	got, err := ToUTF8(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !bytes.Equal(got, []byte("AAAA")) {
		t.Fatalf("got % x, want AAAA", got)
	}
}

func TestApplyToCacheCursorMove(t *testing.T) {
	// 'A' 写入 pos0；ESC [ 2 ` 把写指针移到绝对位置 2；随后 'B' 落在 pos2，
	// 中间 pos1 由 C 侧预分配缓存的零值补齐 -> "A\x00B"。
	in := []byte{'A', 0x1B, 0x5B, 0x32, 0x60, 'B'}
	got, err := ApplyToCache(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !bytes.Equal(got, []byte{'A', 0x00, 'B'}) {
		t.Fatalf("got % x, want 41 00 42", got)
	}
}
