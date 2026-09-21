/**
 * [INPUT]: 依赖 encoding/hex, hash, math/big, regexp
 * [OUTPUT]: 对外提供 padTo, padToN, hashToInt, intFromBytes, bytesFromHexString 等辅助工具函数
 * [POS]: internal/srp 的大数与字节流对齐辅助模块
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package srp

import (
	"encoding/hex"
	"hash"
	"math/big"
	"regexp"
)

func padTo(bytes []byte, length int) []byte {
	paddingLength := length - len(bytes)
	if paddingLength <= 0 {
		return bytes
	}
	padding := make([]byte, paddingLength)

	return append(padding, bytes...)
}

func padToN(number *big.Int, params *SRPParams) []byte {
	return padTo(number.Bytes(), params.NLengthBits/8)
}

func hashToBytes(h hash.Hash) []byte {
	return h.Sum(nil)
}

func hashToInt(h hash.Hash) *big.Int {
	U := new(big.Int)
	U.SetBytes(hashToBytes(h))
	return U
}

func intFromBytes(bytes []byte) *big.Int {
	i := new(big.Int)
	i.SetBytes(bytes)
	return i
}

func intToBytes(i *big.Int) []byte {
	return i.Bytes()
}

func bytesFromHexString(s string) []byte {
	re, _ := regexp.Compile("[^0-9a-fA-F]")
	h := re.ReplaceAll([]byte(s), []byte(""))
	b, _ := hex.DecodeString(string(h))
	return b
}
