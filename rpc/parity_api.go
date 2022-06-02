package rpc

import (
	"context"

	"github.com/openweb3/web3go/types"
)

type parityAPI struct{}

func (api *parityAPI) GetBlockReceipts(ctx context.Context, blockNumOrHash *types.BlockNumberOrHash) ([]types.Receipt, error) {
	return GetEthClientFromContext(ctx).Parity.BlockReceipts(blockNumOrHash)
}
