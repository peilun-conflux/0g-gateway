// Package chain is the real 0G backend: BatchUpload submits many files in a
// single Flow transaction, FileStatus aggregates zgs_getFileInfo across the
// configured nodes, Download restores files with merkle-proof verification.
package chain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/0gfoundation/0g-storage-client/common/blockchain"
	"github.com/0gfoundation/0g-storage-client/core"
	"github.com/0gfoundation/0g-storage-client/node"
	"github.com/0gfoundation/0g-storage-client/transfer"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/openweb3/web3go"

	"zgs-gateway/internal/uploader"
)

type Options struct {
	Nodes           []string // storage node JSON-RPC URLs
	EthRPC          string   // host chain RPC (Conflux eSpace, etc.)
	PrivateKey      string   // submitter key, hex without 0x
	ExpectedReplica uint     // replicas to expect when uploading (default 1)
}

type Backend struct {
	w3       *web3go.Client
	up       *transfer.Uploader
	upCloser func()
	clients  []*node.ZgsClient
	dl       *transfer.Downloader
	replica  uint
}

func New(ctx context.Context, opt Options) (*Backend, error) {
	if len(opt.Nodes) == 0 {
		return nil, errors.New("no storage nodes configured")
	}
	if opt.ExpectedReplica == 0 {
		opt.ExpectedReplica = 1
	}

	// Validate the key up front: the SDK's web3/signer constructors fatally
	// exit (logrus.Fatal → os.Exit) on a malformed key, which would bypass
	// this function's error return and the caller's deferred cleanup.
	if _, err := crypto.HexToECDSA(strings.TrimPrefix(opt.PrivateKey, "0x")); err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	w3, err := blockchain.NewWeb3(opt.EthRPC, opt.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("connect eth rpc: %w", err)
	}
	up, closer, err := transfer.NewUploaderFromConfig(ctx, w3, transfer.UploaderConfig{Nodes: opt.Nodes})
	if err != nil {
		w3.Close()
		return nil, fmt.Errorf("create uploader: %w", err)
	}
	clients := make([]*node.ZgsClient, 0, len(opt.Nodes))
	for _, url := range opt.Nodes {
		c, err := node.NewZgsClient(url, nil)
		if err != nil {
			closer()
			w3.Close()
			for _, cc := range clients {
				cc.Close()
			}
			return nil, fmt.Errorf("connect storage node %s: %w", url, err)
		}
		clients = append(clients, c)
	}
	dl, err := transfer.NewDownloader(clients)
	if err != nil {
		closer()
		w3.Close()
		for _, c := range clients {
			c.Close()
		}
		return nil, fmt.Errorf("create downloader: %w", err)
	}
	return &Backend{w3: w3, up: up, upCloser: closer, clients: clients, dl: dl, replica: opt.ExpectedReplica}, nil
}

func (b *Backend) Close() {
	if b.upCloser != nil {
		b.upCloser()
	}
	for _, c := range b.clients {
		c.Close()
	}
	b.w3.Close()
}

// BatchUpload submits all items in one Flow transaction and uploads their
// segments. The cache files are uploaded as plain bytes (TransactionPacked
// finality; the worker's PollFinality tracks finalization separately).
func (b *Backend) BatchUpload(ctx context.Context, items []uploader.Item) (string, error) {
	datas := make([]core.IterableData, 0, len(items))
	dataOpts := make([]transfer.UploadOption, 0, len(items))
	var files []*core.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for _, it := range items {
		f, err := core.Open(it.Path)
		if err != nil {
			return "", fmt.Errorf("open cache file for %s: %w", it.Root, err)
		}
		files = append(files, f)
		datas = append(datas, f)
		dataOpts = append(dataOpts, transfer.UploadOption{
			FinalityRequired: transfer.TransactionPacked,
			ExpectedReplica:  b.replica,
			SkipTx:           it.SkipTx,
		})
	}

	txHash, roots, err := b.up.BatchUpload(ctx, datas, transfer.BatchUploadOption{DataOptions: dataOpts})
	if err != nil {
		return "", err
	}
	// A root mismatch means the bytes 0G stored are not the bytes the gateway
	// addressed: fail the batch rather than recording the gateway root as
	// onchain. The worker reconciles and retries from this error.
	if len(roots) != len(items) {
		return "", fmt.Errorf("0g returned %d roots for %d uploaded items", len(roots), len(items))
	}
	for i, r := range roots {
		if !strings.EqualFold(r.Hex(), items[i].Root) {
			return "", fmt.Errorf("root mismatch at index %d: gateway computed %s, 0g returned %s",
				i, items[i].Root, r.Hex())
		}
	}
	if (txHash == common.Hash{}) {
		// All items were SkipTx (segments-only re-upload); no new transaction
		// was submitted. Return an empty hash so the worker keeps each object's
		// existing txHash instead of overwriting it with the zero hash.
		return "", nil
	}
	return txHash.Hex(), nil
}

// FileStatus aggregates zgs_getFileInfo across all configured nodes. A single
// node reporting finalized is treated as finalized: this reaches the finalized
// state fastest and never wedges when a node lags, which matters more here than
// confirming durable replication across every node.
func (b *Backend) FileStatus(ctx context.Context, rootHex string) (uploader.FileStatus, error) {
	root := common.HexToHash(rootHex)
	var sawUploading, sawPruned bool
	var lastErr error
	reachable := 0
	for _, c := range b.clients {
		info, err := c.GetFileInfo(ctx, root, false)
		if err != nil {
			lastErr = err
			continue
		}
		reachable++
		switch {
		case info == nil:
			// this node has not seen the tx yet
		case info.Pruned:
			sawPruned = true
		case info.Finalized:
			return uploader.FileFinalized, nil
		default:
			sawUploading = true
		}
	}
	if reachable == 0 {
		return uploader.FileUnknown, fmt.Errorf("all storage nodes unreachable: %w", lastErr)
	}
	if sawUploading {
		return uploader.FileUploading, nil
	}
	if sawPruned {
		return uploader.FilePruned, nil
	}
	return uploader.FileUnknown, nil
}

// Download restores an object into dest with merkle-proof verification.
func (b *Backend) Download(ctx context.Context, root, dest string) error {
	return b.dl.Download(ctx, root, dest, true)
}
