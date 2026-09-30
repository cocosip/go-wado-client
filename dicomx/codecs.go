package dicomx

// 空导入注册 go-dicom-codecs 的全部编解码器到 go-dicom 全局注册表
// （各包 init() 自注册）。覆盖：RLE、JPEG Baseline/Extended/Lossless 族、
// JPEG-LS、JPEG 2000（含 HTJ2K）。
import (
	// 空导入通过各包 init() 自注册，将 go-dicom-codecs 的解码器注册进
	// go-dicom 全局注册表；缺少某个导入则对应传输语法无法解码。
	_ "github.com/cocosip/go-dicom-codecs/jpeg/baseline"       // JPEG Baseline（1.2.840.10008.1.2.4.50）
	_ "github.com/cocosip/go-dicom-codecs/jpeg/extended"       // JPEG Extended（1.2.840.10008.1.2.4.51）
	_ "github.com/cocosip/go-dicom-codecs/jpeg/lossless"       // JPEG Lossless（1.2.840.10008.1.2.4.57）
	_ "github.com/cocosip/go-dicom-codecs/jpeg/lossless14sv1"  // JPEG Lossless Selection Value 1（1.2.840.10008.1.2.4.70）
	_ "github.com/cocosip/go-dicom-codecs/jpeg/standard"       // 基础 JPEG 编解码
	_ "github.com/cocosip/go-dicom-codecs/jpeg2000/htj2k"      // High-Throughput JPEG 2000（1.2.840.10008.1.2.4.201/202）
	_ "github.com/cocosip/go-dicom-codecs/jpeg2000/lossless"   // JPEG 2000 无损（1.2.840.10008.1.2.4.90）
	_ "github.com/cocosip/go-dicom-codecs/jpeg2000/lossy"      // JPEG 2000 有损（1.2.840.10008.1.2.4.91）
	_ "github.com/cocosip/go-dicom-codecs/jpegls/lossless"     // JPEG-LS 无损（1.2.840.10008.1.2.4.80）
	_ "github.com/cocosip/go-dicom-codecs/jpegls/nearlossless" // JPEG-LS 近无损（1.2.840.10008.1.2.4.81）
	_ "github.com/cocosip/go-dicom-codecs/rle"                 // RLE 无损（1.2.840.10008.1.2.5）
)
