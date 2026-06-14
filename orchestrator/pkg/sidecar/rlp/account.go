// Package rlp wraps the go-ethereum RLP decoders with sidecar-specific helpers.
//
// AccountDecoder mirrors Nethermind.Core/Eip2929/AccountDecoder: decode the
// canonical [nonce, balance, storageRoot, codeHash] sequence emitted by NM's
// FlatInTrie leaf values, with the same emptiness semantics so the sidecar's
// `contractsTotal` / `emptyAccounts` counters match the C# plugin bit-for-bit.
package rlp

import (
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// Constants mirroring Nethermind's canonical hashes.
var (
	// EmptyCodeHash = keccak256("").
	EmptyCodeHash = [32]byte{
		0xc5, 0xd2, 0x46, 0x01, 0x86, 0xf7, 0x23, 0x3c,
		0x92, 0x7e, 0x7d, 0xb2, 0xdc, 0xc7, 0x03, 0xc0,
		0xe5, 0x00, 0xb6, 0x53, 0xca, 0x82, 0x27, 0x3b,
		0x7b, 0xfa, 0xd8, 0x04, 0x5d, 0x85, 0xa4, 0x70,
	}
	// EmptyStorageRoot = keccak256(rlp("")) = empty trie root.
	EmptyStorageRoot = [32]byte{
		0x56, 0xe8, 0x1f, 0x17, 0x1b, 0xcc, 0x55, 0xa6,
		0xff, 0x83, 0x45, 0xe6, 0x92, 0xc0, 0xf8, 0x6e,
		0x5b, 0x48, 0xe0, 0x1b, 0x99, 0x6c, 0xad, 0xc0,
		0x01, 0x62, 0x2f, 0xb5, 0xe3, 0x63, 0xb4, 0x21,
	}
)

// AccountInfo is the slim record the sidecar carries per state leaf.
type AccountInfo struct {
	HasCode    bool
	HasStorage bool
	IsEmpty    bool // nonce == 0, balance == 0, no storage, no code
	CodeHash   [32]byte
}

// DecodeAccount decodes a [nonce, balance, storageRoot, codeHash] RLP list.
// Returns (info, true) on success, (zero, false) on any malformed input —
// callers must increment their malformed counter and continue.
func DecodeAccount(buf []byte) (AccountInfo, bool) {
	var info AccountInfo
	listContent, _, err := gethrlp.SplitList(buf)
	if err != nil {
		return info, false
	}
	nonceContent, rest, err := gethrlp.SplitString(listContent)
	if err != nil {
		return info, false
	}
	balanceContent, rest, err := gethrlp.SplitString(rest)
	if err != nil {
		return info, false
	}
	storageRootContent, rest, err := gethrlp.SplitString(rest)
	if err != nil {
		return info, false
	}
	codeHashContent, _, err := gethrlp.SplitString(rest)
	if err != nil {
		return info, false
	}

	nonceIsZero := len(nonceContent) == 0
	balanceIsZero := len(balanceContent) == 0

	if len(storageRootContent) == 32 {
		var sr [32]byte
		copy(sr[:], storageRootContent)
		info.HasStorage = sr != EmptyStorageRoot
	}
	if len(codeHashContent) == 32 {
		copy(info.CodeHash[:], codeHashContent)
		info.HasCode = info.CodeHash != EmptyCodeHash
	}
	info.IsEmpty = nonceIsZero && balanceIsZero && !info.HasStorage && !info.HasCode
	return info, true
}

// NodeKind classifies the top-level shape of a Merkle trie node's RLP encoding.
type NodeKind byte

const (
	NodeInvalid NodeKind = iota
	NodeBranch
	NodeExtension
	NodeLeaf
)

// Classify mirrors Nethermind.StateComposition/Bootstrap/TrieNodeScanner.Classify.
// For leaf nodes leafValue points at the raw inner value bytes; for other kinds it is nil.
func Classify(nodeRLP []byte) (kind NodeKind, leafValue []byte) {
	if len(nodeRLP) == 0 {
		return NodeInvalid, nil
	}
	listContent, _, err := gethrlp.SplitList(nodeRLP)
	if err != nil {
		return NodeInvalid, nil
	}
	count, err := gethrlp.CountValues(listContent)
	if err != nil {
		return NodeInvalid, nil
	}
	if count == 17 {
		return NodeBranch, nil
	}
	if count != 2 {
		return NodeInvalid, nil
	}
	_, firstContent, rest, err := gethrlp.Split(listContent)
	if err != nil {
		return NodeInvalid, nil
	}
	if len(firstContent) == 0 {
		return NodeExtension, nil
	}
	if (firstContent[0] & 0x20) == 0 {
		return NodeExtension, nil
	}
	_, valContent, _, err := gethrlp.Split(rest)
	if err != nil {
		return NodeLeaf, nil
	}
	return NodeLeaf, valContent
}
