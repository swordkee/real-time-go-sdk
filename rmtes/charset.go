package rmtes

// 本文件对应 C 侧 rsslCharSet.h / rsslCharSet.c 的字符集描述。
// 结构体字段、取值与 initWorkingSet 的初始状态逐字对齐 C 实现。
//
// 来源：LSEG Real-Time-SDK（Apache-2.0），commit 7d219a600
//   Cpp-C/Eta/Impl/Codec/rtr/rsslCharSet.h
//   Cpp-C/Eta/Impl/Codec/rsslCharSet.c

const (
	shape94 = 94
	shape96 = 96
)

// charSet 对应 RsslRmtesCharSet。
type charSet struct {
	table1       []uint16
	table2       []uint16
	shape        int
	table1Length uint32
	table2Start  uint32
	table2Length uint32
	stride       int
}

// workingSet 对应 RsslRmtesWorkingSet。
// gl/gr 为二级指针，模拟 C 的 RsslRmtesCharSet **GL/**GR：
// 转义序列可把 GL/GR 改指 g0~g3 中任一个，同时 ESC $ ... 可改写 g0~g3 本身。
type workingSet struct {
	g0, g1, g2, g3 *charSet
	gl, gr         **charSet
}

// 必须返回指针：gl/gr 是指向 g0~g3 字段的二级指针，
// 若按值返回，拷贝出的 workingSet 的 gl 会仍指向原局部结构体的字段。
func newWorkingSet() *workingSet {
	ws := &workingSet{
		g0: &charsetReuterBasic1,
		g1: &charsetReuterBasic2,
		g2: &charsetJapaneseKatakana,
		g3: &charsetJapaneseKanji,
	}
	ws.gl = &ws.g0
	ws.gr = &ws.g1
	return ws
}

func (ws *workingSet) glSet() *charSet { return *ws.gl }
func (ws *workingSet) grSet() *charSet { return *ws.gr }

// 七个内置字符集，字段值来自 rsslCharSet.c 的 RsslRmtesCharSet 常量定义。
var (
	charsetReuterBasic1 = charSet{
		table1: nil, table2: nil, shape: shape94,
		table1Length: 94, table2Start: 0, table2Length: 1, stride: 0,
	}
	charsetReuterBasic2 = charSet{
		table1: tblRBCS2[:], table2: tblRBCS2BR[:], shape: shape94,
		table1Length: 94, table2Start: 63, table2Length: 1, stride: 0,
	}
	charsetJapaneseKatakana = charSet{
		table1: tblKatakana[:], table2: nil, shape: shape94,
		table1Length: 63, table2Start: 0, table2Length: 1, stride: 0,
	}
	charsetJapaneseLatin = charSet{
		table1: tblRomaji[:], table2: nil, shape: shape94,
		table1Length: 94, table2Start: 0, table2Length: 1, stride: 0,
	}
	charsetJapaneseKanji = charSet{
		table1: tbl208a[:], table2: tbl208b[:], shape: shape94,
		table1Length: (0x28-0x21)*94 + (0x40 - 0x21) + 1,
		table2Start:  (0x30 - 0x21) * 94,
		table2Length: (0x74-0x30)*94 + (0x26 - 0x21) + 1,
		stride:       2,
	}
	charsetChinese1 = charSet{
		table1: tblCNS1a[:], table2: tblCNS1b[:], shape: shape94,
		table1Length: (0x26-0x21)*94 + (0x3E - 0x21) + 1,
		table2Start:  (0x42 - 0x21) * 94,
		table2Length: (0x7D-0x42)*94 + (0x4B - 0x21) + 1,
		stride:       2,
	}
	charsetChinese2 = charSet{
		table1: tblCNS2[:], table2: nil, shape: shape94,
		table1Length: (0x72-0x21)*94 + (0x44 - 0x21) + 1,
		table2Start:  0, table2Length: 0, stride: 2,
	}
)

func glLowest(s *charSet) int {
	if s.shape == shape96 {
		return 0x20
	}
	return 0x21
}

func glHighest(s *charSet) int {
	if s.shape == shape96 {
		return 0x7F
	}
	return 0x7E
}

func grLowest(s *charSet) int {
	if s.shape == shape96 {
		return 0xA0
	}
	return 0xA1
}

func grHighest(s *charSet) int {
	if s.shape == shape96 {
		return 0xFF
	}
	return 0xFE
}

func isGLChar(c byte, s *charSet) bool {
	return glLowest(s) <= int(c) && int(c) <= glHighest(s)
}

func isGRChar(c byte, s *charSet) bool {
	return grLowest(s) <= int(c) && int(c) <= grHighest(s)
}

func glConvertSingle1(c byte, s *charSet) uint16 {
	if s.table1 != nil {
		return s.table1[int(c)-glLowest(s)]
	}
	return uint16(c)
}

func glConvertSingle2(c byte, s *charSet) uint16 {
	return s.table2[int(c)-glLowest(s)]
}

func grConvertSingle1(c byte, s *charSet) uint16 {
	if s.table1 != nil {
		return s.table1[int(c)-grLowest(s)]
	}
	return uint16(c)
}

func grConvertSingle2(c byte, s *charSet) uint16 {
	return s.table2[int(c)-grLowest(s)]
}

func convertStride2GL(c1, c2 byte, s *charSet) uint16 {
	idx := uint32(int(c1)-glLowest(s))*94 + uint32(int(c2)-glLowest(s))
	if idx < s.table1Length {
		return s.table1[idx]
	}
	if s.table2 != nil && idx >= s.table2Start && idx < s.table2Start+s.table2Length {
		return s.table2[idx-s.table2Start]
	}
	return 0xFFFD
}

func convertStride2GR(c1, c2 byte, s *charSet) uint16 {
	idx := uint32(int(c1)-grLowest(s))*94 + uint32(int(c2)-grLowest(s))
	if idx < s.table1Length {
		return s.table1[idx]
	}
	if s.table2 != nil && idx >= s.table2Start && idx < s.table2Start+s.table2Length {
		return s.table2[idx-s.table2Start]
	}
	return 0xFFFD
}
