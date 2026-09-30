// Package dicomx 是 WADO 客户端与 cocosip/go-dicom（及 go-dicom-codecs）的集成胶水。
//
// DICOM 的解析/序列化全部委托 go-dicom，不自行实现：
//   - 元数据（dicom+json）由 wadors 直接走 go-dicom 的 serialization 包；
//   - DICOM 文件解析走本包对 multipart 响应的流式桥接（Datasets）；
//   - 像素编解码器（RLE/JPEG/JPEG-LS/JPEG 2000/HTJ2K）经本包 codecs.go
//     空导入注册到 go-dicom 全局注册表。
//
// 不需要像素编解码时可不导入本包，保持二进制轻量。
package dicomx

import (
	"fmt"
	"iter"

	dparser "github.com/cocosip/go-dicom/pkg/dicom/parser"
	"github.com/cocosip/go-wado-client/wadors"
)

// Datasets 流式解析 WADO-RS multipart 响应中的每个 DICOM part。
// opts 透传 go-dicom parser 选项（如 parser.WithReadOption(parser.SkipLargeTags)）。
// 任一 part 解析失败即停止迭代并返回该错误。
func Datasets(mp *wadors.Multipart, opts ...dparser.Option) iter.Seq2[*dparser.ParseResult, error] {
	return func(yield func(*dparser.ParseResult, error) bool) {
		for part, err := range mp.Parts() {
			if err != nil {
				yield(nil, err)
				return
			}
			res, err := dparser.Parse(part, opts...)
			if err != nil {
				yield(nil, fmt.Errorf("dicomx: parse part %d: %w", part.Index(), err))
				return
			}
			if !yield(res, nil) {
				return
			}
		}
	}
}
