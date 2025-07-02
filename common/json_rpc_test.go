package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ethGetBlockByNumberResponseCanonicalHash = "9f31e9ad0f518cefc3db028100ae0b6a8ae52651dee0abb66917149523a676e7"
const ethGetBlockByNumberResponse string = `{"id":1,"jsonrpc":"2.0","result":{"baseFeePerGas":"0x137d0bb9e","blobGasUsed":"0xa0000","difficulty":"0x0","excessBlobGas":"0x60000","extraData":"0x626573752032352e352d646576656c6f702d62386364346338","gasLimit":"0x390d860","gasUsed":"0x15cfc87","hash":"0xe27565f06f04fe79d3c3bb4dc9749a0318c520d7f784545be4d1a65bbcac21db","logsBloom":"0xae206c5f8adfa784b2aca218aa9c9eb470f714451e08f0efe0ea2406359615c3839abc07ba5e3c1a0102168b1d2fb858c132376084e6dc04fe42be6624ec249b0a9ab88485a2f52cc4297ccf7f23fb3885596b5464562ce84b9b2955e1a080917d0032833bed3c4101a89c4bf3434d4e16411fc0c0244f497386dc3c11f583cce2d58635f73ac75464c840a7e75cf03df174a649f08caf4e9a153188fe862cdac6d82bd22fc5358b5fe624e65be4028848a2f40aa2abb89a391e930a616d08adfc5960b209c1658a0b1f60f1fc63a0b41857c91488e50a6b00031de20948eae7cc1abf18c99da0cb678d791b987a6e1ce8268b34c432564853b6a52128726e06","miner":"0x3826539cbd8d68dcf119e80b994557b4278cec9f","mixHash":"0x807bee076a500d33eb547cdcf948808b473a8ed44f6227b56647f64719340b94","nonce":"0x0000000000000000","number":"0x7f04f1","parentBeaconBlockRoot":"0xc0959914345b1f080a0659d7ff38cc1a9992caf800b62de6b2dd5b548f072984","parentHash":"0xf60f120fe84500833ef563123067a765361a7af59a11e3370971c1136e28540d","receiptsRoot":"0x6c3c22526eaf8eb9e177d60e7aee4f546e79527468fd294d1fde9f1e3bcc17ee","requestsHash":"0xe3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","sha3Uncles":"0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347","size":"0x1faa2","stateRoot":"0xb40115a391949e36c4cf558d93686537360cd9c4193fb2c0932a4812c340437f","timestamp":"0x68246b68","transactions":["0xdb56e3aea5cf9dde901471cf8b001de8f21246a9ee529e7f084af6919619fdcb","0x8f2c922d06201010b7b46559c500f265cd4d9eb1920a548365ac8a956d8409fe"],"transactionsRoot":"0x7302699e65a901d688e8a0f06aeb1bf7aeed7445c27e70e4205da9c90b5a7edd","uncles":[],"withdrawals":[{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x38e71","index":"0x515d3ad","validatorIndex":"0x19b"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x342cf","index":"0x515d3ae","validatorIndex":"0x19c"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x342cf","index":"0x515d3af","validatorIndex":"0x19d"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x38e71","index":"0x515d3b0","validatorIndex":"0x19e"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x342cf","index":"0x515d3b1","validatorIndex":"0x19f"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x3da13","index":"0x515d3b2","validatorIndex":"0x1a0"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x9010","index":"0x515d3b3","validatorIndex":"0x1a4"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x4808","index":"0x515d3b4","validatorIndex":"0x1a6"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x4808","index":"0x515d3b5","validatorIndex":"0x1ab"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x4808","index":"0x515d3b6","validatorIndex":"0x1ad"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x446e","index":"0x515d3b7","validatorIndex":"0x1b4"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x88dc","index":"0x515d3b8","validatorIndex":"0x1bb"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x446e","index":"0x515d3b9","validatorIndex":"0x1bd"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x88dc","index":"0x515d3ba","validatorIndex":"0x1c0"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x81a8","index":"0x515d3bb","validatorIndex":"0x1cf"},{"address":"0x25c4a76e7d118705e7ea2e9b7d8c59930d8acd3b","amount":"0x3d3a","index":"0x515d3bc","validatorIndex":"0x1d6"}],"withdrawalsRoot":"0xba38ea1816871bfbd95cccc97160a762b06d9a654135477ea832662eb6748acd"}}`
const ethGetLogsResponseCanonicalHash = "bbcaca9fcf5a3e493244d5ea5edeaeac7d01fb932134b9afbea74beeafa52653"
const ethGetLogsResponse string = `{"jsonrpc":"2.0","id":1,"result":[{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x00000000000000000000000056eddb7aa87536c09ccc2793473599fd21a8b17f","0x000000000000000000000000e291cc3e5b9e0c9b37c9fbdd549abf3b5c0ad342"],"data":"0x0000000000000000000000000000000000000000000000000000000ba42490a0","blockNumber":"0x15536ea","transactionHash":"0x9ebd9461d1973d565c0a53c054e3eb058b1b2a14d0982cea11db488b455a7dab","transactionIndex":"0x11","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x2b","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000006240b5dd984285ead016030c734f709131d18d36","0x0000000000000000000000002552a7660a03b35f05af5bc8c0173794dfb19886"],"data":"0x0000000000000000000000000000000000000000000000000000000006567026","blockNumber":"0x15536ea","transactionHash":"0x4b89ba745b492f58f13a61e4439d737d4aa9148b2e724c00e662bac084a08218","transactionIndex":"0x16","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x2c","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000dfd5293d8e347dfe59e90efd55b2956a1343963d","0x00000000000000000000000089f11e5d209fa14e4c3a32d1c6415a277b2efd02"],"data":"0x00000000000000000000000000000000000000000000000000000000030291a0","blockNumber":"0x15536ea","transactionHash":"0xfb5ddb6a645715d2ce34e02db73ca26cf996a61501fca847a0ab382da8cea1af","transactionIndex":"0x1e","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x31","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x00000000000000000000000021a31ee1afc51d94c2efccaa2092ad1028285549","0x0000000000000000000000006cda56673c4d3c6fe21b787d4e5abbc209519c4c"],"data":"0x0000000000000000000000000000000000000000000000000000000005defda0","blockNumber":"0x15536ea","transactionHash":"0x86a5bd78d3dc4fd44a343a32442ba6a360c898854760992b5f07a910c61662b4","transactionIndex":"0x20","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x32","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000fca00b1d6dc6576bfd59dcf4f22b24c818dd1301","0x000000000000000000000000489d525e7ce5aa55cde521e1f84fb5890ca5e883"],"data":"0x0000000000000000000000000000000000000000000000000000000039292dc0","blockNumber":"0x15536ea","transactionHash":"0xa64f0d2f1e1cd97283afee7eb8c835ffc4afe6ba84acec36409765b006ffe055","transactionIndex":"0x22","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x3a","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000b82fe5b9b05bc4196a7a572cde5f35fd21f97f15","0x0000000000000000000000008c97231a9def78c633be883b3d1886e1408552e1"],"data":"0x000000000000000000000000000000000000000000000000000000000660b0c0","blockNumber":"0x15536ea","transactionHash":"0xb10188a77865c27a98605bf94f5dd7ee45bde0dfa94f33c0e0d1dc385ea42830","transactionIndex":"0x23","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x3b","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x00000000000000000000000046340b20830761efd32832a74d7169b29feb9758","0x000000000000000000000000e2803bc1099eeab4322476bcc38f12ead6d4a98e"],"data":"0x00000000000000000000000000000000000000000000000000000000052c2bde","blockNumber":"0x15536ea","transactionHash":"0x69431068335b06024270e8fd1b01e3059c75ab6f43e2e7eef1babfef87c766d3","transactionIndex":"0x26","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x49","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000a551e70a4374e9369a89f5888a04cc6ac73706d1","0x0000000000000000000000000e9ff1524e75d53cb16a68cadbd8a96f0e172d5a"],"data":"0x0000000000000000000000000000000000000000000000000000000077359400","blockNumber":"0x15536ea","transactionHash":"0x6b51b8a9021b1a70746a942fb44292d113f43ce7e86639e0818cb2eb0dfa7a1b","transactionIndex":"0x3b","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x6e","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000cf24d8a93c9277b4a15f057d65e8bd91733f12e0","0x0000000000000000000000009008d19f58aabd9ed0d60971565aa8510560ab41"],"data":"0x000000000000000000000000000000000000000000000000000000000ac258ba","blockNumber":"0x15536ea","transactionHash":"0x2fb1c780ae676fcd034f19e52f8234636e43ba12da4030d914bd9232bc48ca23","transactionIndex":"0x3c","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x85","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000009008d19f58aabd9ed0d60971565aa8510560ab41","0x000000000000000000000000e08da0d972b74581ec77a3c988e47d6e56bfbb7d"],"data":"0x000000000000000000000000000000000000000000000000000000000b176785","blockNumber":"0x15536ea","transactionHash":"0x2fb1c780ae676fcd034f19e52f8234636e43ba12da4030d914bd9232bc48ca23","transactionIndex":"0x3c","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x89","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000009a1187cb7084f3e60a8b99eb195d9f3c29361a8a","0x0000000000000000000000008dca941474e2f9378157fec599d02025dd87eb09"],"data":"0x0000000000000000000000000000000000000000000000000000000000b71b00","blockNumber":"0x15536ea","transactionHash":"0x0623d92fdafc0b9c60dffb66366f9fdbb072a593a9a5fddc081ccfad3a92a629","transactionIndex":"0x3e","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x8d","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x00000000000000000000000006c08f16eca3bde3df6dad9c8ecd836198a72560","0x000000000000000000000000a0bb1ebf52a9307f30509d3b385754c33b7f2e26"],"data":"0x000000000000000000000000000000000000000000000000000000021986cfc0","blockNumber":"0x15536ea","transactionHash":"0x281325c411cd42c3a53439412d75acfcfd10625121aaabcac0230049a41f83a6","transactionIndex":"0x40","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x8f","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x00000000000000000000000092e6f7ad96f939b3cccb416dc32d621e73efd9c0","0x0000000000000000000000009866b529c14ac1289a5c1682d400bdea0906a6e8"],"data":"0x000000000000000000000000000000000000000000000000000000083ffd1282","blockNumber":"0x15536ea","transactionHash":"0x7adc0503f808e9a2a874cd1801702d9743133013df40c5b12886b0311d059d4b","transactionIndex":"0x46","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x94","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000000000000004444c5dc75cb358380d2e3de08a90","0x0000000000000000000000000f4a1d7fdf4890be35e71f3e0bbc4a0ec377eca3"],"data":"0x000000000000000000000000000000000000000000000000000000007798eafc","blockNumber":"0x15536ea","transactionHash":"0xb85e8e8127968d490c33c781bb0c28846e6e85ca2f60bf7feab7e6407899cf92","transactionIndex":"0x49","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0xa7","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000000f4a1d7fdf4890be35e71f3e0bbc4a0ec377eca3","0x000000000000000000000000e0e0e08a6a4b9dc7bd67bcb7aade5cf48157d444"],"data":"0x000000000000000000000000000000000000000000000000000000007798eafc","blockNumber":"0x15536ea","transactionHash":"0xb85e8e8127968d490c33c781bb0c28846e6e85ca2f60bf7feab7e6407899cf92","transactionIndex":"0x49","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0xab","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000c7bbec68d12a0d1830360f8ec58fa599ba1b0e9b","0x0000000000000000000000000f4a1d7fdf4890be35e71f3e0bbc4a0ec377eca3"],"data":"0x0000000000000000000000000000000000000000000000000000000077916f48","blockNumber":"0x15536ea","transactionHash":"0xb85e8e8127968d490c33c781bb0c28846e6e85ca2f60bf7feab7e6407899cf92","transactionIndex":"0x49","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0xad","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000000f4a1d7fdf4890be35e71f3e0bbc4a0ec377eca3","0x000000000000000000000000e0e0e08a6a4b9dc7bd67bcb7aade5cf48157d444"],"data":"0x0000000000000000000000000000000000000000000000000000000077916f48","blockNumber":"0x15536ea","transactionHash":"0xb85e8e8127968d490c33c781bb0c28846e6e85ca2f60bf7feab7e6407899cf92","transactionIndex":"0x49","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0xb3","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000005bcbdfb6cc624b959c39a2d16110d1f2d9204f72","0x000000000000000000000000f20d84e89291bd2b117d04e43b05c1a3aa3652e8"],"data":"0x000000000000000000000000000000000000000000000000000000000444a5f8","blockNumber":"0x15536ea","transactionHash":"0xc51bc897553e28cef0a517c92e4ede9bdd9485e0dc9889692fc68eee762fd90c","transactionIndex":"0x4d","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0xe1","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000003ed41b1f9b6263d37f6164f457fcee5a3462a113","0x000000000000000000000000797bda0f9ed32cf3c0c5c2ea7ead603c60fe61b4"],"data":"0x000000000000000000000000000000000000000000000000000000003b9aca00","blockNumber":"0x15536ea","transactionHash":"0x4268f7591d237390120d787926169d6c85829c86dbec65bb0d3c7518907f5961","transactionIndex":"0x4e","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0xe2","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000000000000004444c5dc75cb358380d2e3de08a90","0x00000000000000000000000081463b0f960f247f704377661ec81c1fd65b5128"],"data":"0x0000000000000000000000000000000000000000000000000000000004d363a7","blockNumber":"0x15536ea","transactionHash":"0x17a9334180e2ee37d253412337a6474779921988b86a1bd6321332c30c9811da","transactionIndex":"0x52","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0xf0","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x00000000000000000000000081463b0f960f247f704377661ec81c1fd65b5128","0x000000000000000000000000cf24d8a93c9277b4a15f057d65e8bd91733f12e0"],"data":"0x0000000000000000000000000000000000000000000000000000000004d363a7","blockNumber":"0x15536ea","transactionHash":"0x17a9334180e2ee37d253412337a6474779921988b86a1bd6321332c30c9811da","transactionIndex":"0x52","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0xf1","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000005ba52d7300719aa4eea45acefcf292b3f339c428","0x000000000000000000000000120358307d278981369067222d0457056cd23d00"],"data":"0x000000000000000000000000000000000000000000000000000000000fdcb620","blockNumber":"0x15536ea","transactionHash":"0x6def7e4250847f73e89115a256612392c672d097be26ceaa6ade2c8246fd036f","transactionIndex":"0x5a","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x103","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000006716b8abe28542003fe7d734a54c2f016738b468","0x000000000000000000000000cae0512aac47804e3905a51f0337fdcdb74e96e1"],"data":"0x000000000000000000000000000000000000000000000000000000000001868b","blockNumber":"0x15536ea","transactionHash":"0x126923fbe6b97fd68466dc429d282c1b81cb68ccfa6707b267d567f99b698c20","transactionIndex":"0x68","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x118","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000002095667cd70028d5d30c94897310271ec6a44beb","0x0000000000000000000000000e47c3a84c924a7fc4e22b76c1ab7ec7f32892b4"],"data":"0x0000000000000000000000000000000000000000000000000000000000002711","blockNumber":"0x15536ea","transactionHash":"0xc22c9bbc305788b819411de13606cc9d98d39de1feb3b22e2729696f8a115ffe","transactionIndex":"0x69","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x119","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925","0x0000000000000000000000006aba0315493b7e6989041c91181337b662fb1b90","0x000000000000000000000000b300000b72deaeb607a12d5f54773d1c19c7028d"],"data":"0x00000000000000000000000000000000000000000000000000000000024d3aea","blockNumber":"0x15536ea","transactionHash":"0x7cdd1250b267311c73210429aa6aa1cf24446cf22521a6bb591d1d61b0e1e2d8","transactionIndex":"0x72","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x148","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000006aba0315493b7e6989041c91181337b662fb1b90","0x000000000000000000000000b300000b72deaeb607a12d5f54773d1c19c7028d"],"data":"0x00000000000000000000000000000000000000000000000000000000024d3aea","blockNumber":"0x15536ea","transactionHash":"0x7cdd1250b267311c73210429aa6aa1cf24446cf22521a6bb591d1d61b0e1e2d8","transactionIndex":"0x72","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x149","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925","0x000000000000000000000000b300000b72deaeb607a12d5f54773d1c19c7028d","0x0000000000000000000000001231deb6f5749ef6ce6943a275a1d3e7486f4eae"],"data":"0x00000000000000000000000000000000000000000000000000000000024d3aea","blockNumber":"0x15536ea","transactionHash":"0x7cdd1250b267311c73210429aa6aa1cf24446cf22521a6bb591d1d61b0e1e2d8","transactionIndex":"0x72","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x14a","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000b300000b72deaeb607a12d5f54773d1c19c7028d","0x0000000000000000000000001231deb6f5749ef6ce6943a275a1d3e7486f4eae"],"data":"0x00000000000000000000000000000000000000000000000000000000024d3aea","blockNumber":"0x15536ea","transactionHash":"0x7cdd1250b267311c73210429aa6aa1cf24446cf22521a6bb591d1d61b0e1e2d8","transactionIndex":"0x72","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x14b","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000001231deb6f5749ef6ce6943a275a1d3e7486f4eae","0x000000000000000000000000d52abafd35289749cb1587b53cdd35ebe718d583"],"data":"0x00000000000000000000000000000000000000000000000000000000024d3aea","blockNumber":"0x15536ea","transactionHash":"0x7cdd1250b267311c73210429aa6aa1cf24446cf22521a6bb591d1d61b0e1e2d8","transactionIndex":"0x72","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x14c","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000d52abafd35289749cb1587b53cdd35ebe718d583","0x000000000000000000000000a5407eae9ba41422680e2e00537571bcc53efbfd"],"data":"0x00000000000000000000000000000000000000000000000000000000024d3aea","blockNumber":"0x15536ea","transactionHash":"0x7cdd1250b267311c73210429aa6aa1cf24446cf22521a6bb591d1d61b0e1e2d8","transactionIndex":"0x72","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x14d","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925","0x000000000000000000000000b300000b72deaeb607a12d5f54773d1c19c7028d","0x0000000000000000000000001231deb6f5749ef6ce6943a275a1d3e7486f4eae"],"data":"0x0000000000000000000000000000000000000000000000000000000000000000","blockNumber":"0x15536ea","transactionHash":"0x7cdd1250b267311c73210429aa6aa1cf24446cf22521a6bb591d1d61b0e1e2d8","transactionIndex":"0x72","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x158","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000c7bbec68d12a0d1830360f8ec58fa599ba1b0e9b","0x000000000000000000000000b300000b72deaeb607a12d5f54773d1c19c7028d"],"data":"0x0000000000000000000000000000000000000000000000000000000007afa460","blockNumber":"0x15536ea","transactionHash":"0xc74e4b877f155084699e1dc710e5e80ef70ba53ebdd7e2d79af18169332d9542","transactionIndex":"0x77","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x16c","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000b300000b72deaeb607a12d5f54773d1c19c7028d","0x0000000000000000000000006aba0315493b7e6989041c91181337b662fb1b90"],"data":"0x0000000000000000000000000000000000000000000000000000000007afa460","blockNumber":"0x15536ea","transactionHash":"0xc74e4b877f155084699e1dc710e5e80ef70ba53ebdd7e2d79af18169332d9542","transactionIndex":"0x77","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x16f","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000a35723947d25bdcf0714f19be9bd9779b293128d","0x00000000000000000000000045b7edda606d814048e4ff9884c0b56b0b82a0a6"],"data":"0x000000000000000000000000000000000000000000000000000000000bebd1a0","blockNumber":"0x15536ea","transactionHash":"0x035876cf50ece14e6214a97cb78eeee2e2263e62b03b5f52f8ace584cd34d4d7","transactionIndex":"0xa8","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x1c3","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x00000000000000000000000049c7db08d3e1e56deb12c41d94a31365d9944562","0x000000000000000000000000fead65e9d5fea3d8848bda557995076cc95e8865"],"data":"0x000000000000000000000000000000000000000000000000000000002bd30650","blockNumber":"0x15536ea","transactionHash":"0x8781f3bf36416fcb001ada8c0be0ee5ca91cf9b24d2a2150aec75a894e34c54b","transactionIndex":"0xba","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x1de","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925","0x000000000000000000000000c0cb85af8d14e89d31b2ee4cf74640f5de001a32","0x00000000000000000000000081014f44b0a345033bb2b3b21c7a1a308b35feea"],"data":"0x0000000000000000000000000000000000000000000000000000000097655300","blockNumber":"0x15536ea","transactionHash":"0x612ef0b1e15351fb5e4eaf5ecbf604ce991180ad3791f884a7e843134971979e","transactionIndex":"0xbf","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x1e5","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x0000000000000000000000001c727a55ea3c11b0ab7d3a361fe0f3c47ce6de5d","0x000000000000000000000000827f79cfee1de6f1817734a0b917b9e2f8282ce6"],"data":"0x00000000000000000000000000000000000000000000000000000000019422a1","blockNumber":"0x15536ea","transactionHash":"0x07e22f930fe97b03d151c31d48bf38c00c2dcc338ad6dfcd6a94fcaf0f77e2b2","transactionIndex":"0xc1","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x1e7","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000ce21ce2d6c3b19bff8a84271738965abc85709b7","0x0000000000000000000000002ad831f0c27511f29a9edf83491347aedc2ef586"],"data":"0x0000000000000000000000000000000000000000000000000000000478f4b03d","blockNumber":"0x15536ea","transactionHash":"0x126eb76bd8f6b117f0ba2127d3b78f50f457a1dfe2d4ae13824cb1d397a15283","transactionIndex":"0xc5","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x1eb","removed":false},{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x00000000000000000000000058d70877268e73363bedc432418469e34e172c9e","0x0000000000000000000000003283faf28a3676f19842401b6c66fe586896d817"],"data":"0x000000000000000000000000000000000000000000000000000000000a432b40","blockNumber":"0x15536ea","transactionHash":"0xee4fa67fbc28c0be5ba2f244775ba02a156667f4c22740adee0eed27a554569c","transactionIndex":"0xcb","blockHash":"0xaf8913e030cfbcb24b27dc884b6c0eb383f42c824f341e798f0d51e2850c943e","logIndex":"0x1f1","removed":false}]}`
const debugTraceBlockByNumberResponseCanonicalHash = "9924d1e44a138192e770b02dce28862ecf2c0a2d508a6c0acf77bd13ec87ffa9"
const debugTraceBlockByNumberResponse string = `{"jsonrpc":"2.0","id":1,"result":[{"type":"CALL","from":"0x3b5b09d988ba5be25d130a6959a7e4dc5cad4578","to":"0x7a250d5630b4cf539739df2c5dacb4c659f2488d","value":"0x2386f26fc10000","gas":"0x55730","gasUsed":"0x3d227","input":"0x7ff36ab5000000000000000000000000000000000000000000000000000b8d442e2d8b000000000000000000000000000003b5b09d988ba5be25d130a6959a7e4dc5cad45780000000000000000000000000000000000000000000000000000000064a571e9","output":"0x0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000100000000000000000000000000000000000000000000000000002e90edd00000","calls":[{"type":"CALL","from":"0x7a250d5630b4cf539739df2c5dacb4c659f2488d","to":"0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2","value":"0x2386f26fc10000","gas":"0x12e04","gasUsed":"0x8fc","input":"0xd0e30db0","output":"0x","calls":[]},{"type":"CALL","from":"0x7a250d5630b4cf539739df2c5dacb4c659f2488d","to":"0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2","value":"0x0","gas":"0x11b36","gasUsed":"0x2c33","input":"0xa9059cbb0000000000000000000000005c69bee701ef814a2b6a3edd4b1652cb9cc5aa6f0000000000000000000000000000000000000000000000000002386f26fc10000","output":"0x0000000000000000000000000000000000000000000000000000000000000001","calls":[]}]},{"type":"CALL","from":"0xf04a5cc80b1e94c69b48f5ee68a08cd2f09a7c3e","to":"0x881d40237659c251811cec9c364ef91dc08d300c","value":"0x0","gas":"0x29dbb","gasUsed":"0x16e0a","input":"0x38ed1739000000000000000000000000000000000000000000000000000429d069189e00000000000000000000000000000000000000000000000000029e8d66e8203c2e0000000000000000000000000000000000000000000000000000000000000000a0000000000000000000000000f04a5cc80b1e94c69b48f5ee68a08cd2f09a7c3e0000000000000000000000000000000000000000000000000000000064a571f40000000000000000000000000000000000000000000000000000000000000002000000000000000000000000dac17f958d2ee523a2206206994597c13d831ec70000000000000000000000000d8775f648430679a709e98d2b0cb6250d2887ef","output":"0x0000000000000000000000000000000000000000000000000029e8d66e8203c2e","calls":[]}]}`

func init() {
	util.ConfigureTestLogger()
}

func TestEnsureCachedNode_HeapAllocation(t *testing.T) {
	// Create a JsonRpcResponse
	r := &JsonRpcResponse{
		Result: []byte(`{"id":1,"error":null,"result":"value"}`),
	}

	// First call to ensure we have a cached node
	if err := r.ensureCachedNode(); err != nil {
		t.Fatalf("First ensureCachedNode failed: %v", err)
	}

	// Save the address of the cached node
	originalCachedNode := r.cachedNode

	// Verify it's not nil
	if originalCachedNode == nil {
		t.Fatal("cachedNode is nil after initial call")
	}

	// Make a copy of the node data to compare later
	originalNodeValue := *originalCachedNode

	// Run GC to ensure our node stays alive
	for i := 0; i < 10; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}

	// Second call should use the existing cached node
	if err := r.ensureCachedNode(); err != nil {
		t.Fatalf("Second ensureCachedNode failed: %v", err)
	}

	// Verify the node address stayed the same (proper caching)
	if r.cachedNode != originalCachedNode {
		t.Fatal("cached node address changed on second call")
	}

	// Verify the node content is still intact
	if !reflect.DeepEqual(*r.cachedNode, originalNodeValue) {
		t.Fatal("cached node content changed unexpectedly")
	}
}

func TestJsonRpcResponse_Hash(t *testing.T) {
	t.Parallel()

	tt := []struct {
		name         string
		json         string
		reverseJson  bool
		expectedHash string
	}{
		//hashes can be calculated using printf '{"id":1,"error":null,"result":"123456789"}' | sha256
		// {
		// 	name:         "simple result",
		// 	json:         `{"id":1, "result":"123456789"}`,
		// 	expectedHash: "e5588adf0034da798adcbfb22ce4dbefeb42fc1f1747e77067ade70dc8c2b8df",
		// },
		{
			name:         "eth_getBlockByNumber result",
			json:         ethGetBlockByNumberResponse,
			expectedHash: ethGetBlockByNumberResponseCanonicalHash,
		},
		{
			name:         "eth_getBlockByNumber reversed result",
			json:         ethGetBlockByNumberResponse,
			reverseJson:  true,
			expectedHash: ethGetBlockByNumberResponseCanonicalHash,
		},
		{
			name:         "eth_getLogs result",
			json:         ethGetLogsResponse,
			expectedHash: ethGetLogsResponseCanonicalHash,
		},
		{
			name:         "eth_getLogs reversed result",
			json:         ethGetLogsResponse,
			reverseJson:  true,
			expectedHash: ethGetLogsResponseCanonicalHash,
		},
		{
			name:         "debug_traceBlockByNumber result",
			json:         debugTraceBlockByNumberResponse,
			expectedHash: debugTraceBlockByNumberResponseCanonicalHash,
		},
		{
			name:         "debug_traceBlockByNumber reversed result",
			json:         debugTraceBlockByNumberResponse,
			reverseJson:  true,
			expectedHash: debugTraceBlockByNumberResponseCanonicalHash,
		},
	}
	for _, tc := range tt {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := tc.json
			if tc.reverseJson {
				var err error
				result, err = reverseLexSortJSON(tc.json)
				require.NoError(t, err)
				require.NotEqual(t, result, tc.json)
			}

			// Create a JsonRpcResponse
			r := &JsonRpcResponse{
				Result: []byte(result),
			}

			hash, err := r.CanonicalHash()
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedHash, hash, "expected hash for result %s", result)
		})
	}
}

// ReverseLexSortJSON recursively sorts JSON object keys in reverse lex order
func reverseLexSortJSON(input string) (string, error) {
	var obj interface{}
	if err := json.Unmarshal([]byte(input), &obj); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}

	sorted, err := sortJSONReverse(obj)
	if err != nil {
		return "", err
	}

	return string(sorted), nil
}

// sortJSONReverse recursively sorts JSON keys in reverse lex order
func sortJSONReverse(v interface{}) ([]byte, error) {
	switch v := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))

		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			keyBytes, _ := json.Marshal(k)
			valBytes, err := sortJSONReverse(v[k])
			if err != nil {
				return nil, err
			}
			buf.Write(keyBytes)
			buf.WriteByte(':')
			buf.Write(valBytes)
			if i < len(keys)-1 {
				buf.WriteByte(',')
			}
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil

	case []interface{}:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range v {
			itemBytes, err := sortJSONReverse(item)
			if err != nil {
				return nil, err
			}
			buf.Write(itemBytes)
			if i < len(v)-1 {
				buf.WriteByte(',')
			}
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil

	default:
		return json.Marshal(v)
	}
}

func TestJsonRpcRequest_MarshalParams(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		rawReq, err := SonicCfg.Marshal(JsonRpcRequest{
			JSONRPC: "2.0",
			ID:      1,
			Method:  "eth_blockNumber",
			Params:  []interface{}{},
		})
		assert.NoError(t, err)

		expectedRawReq := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
		assert.Equal(t, expectedRawReq, string(rawReq))
	})

	t.Run("Value", func(t *testing.T) {
		rawReq, err := SonicCfg.Marshal(JsonRpcRequest{
			JSONRPC: "2.0",
			ID:      1,
			Method:  "eth_blockNumber",
			Params:  []interface{}{"0xcafecafecafecafecafecafecafecafecafecafe"},
		})
		assert.NoError(t, err)

		expectedRawReq := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":["0xcafecafecafecafecafecafecafecafecafecafe"]}`
		assert.Equal(t, expectedRawReq, string(rawReq))
	})
}

func TestJsonRpcResponse_CanonicalHash_EmptyishNormalization(t *testing.T) {
	// Test cases that should produce the same hash due to emptyish normalization
	testGroups := []struct {
		name     string
		variants []string
	}{
		{
			name: "empty arrays vs non-existing fields",
			variants: []string{
				`{"result": {"data": "0x123", "logs": []}}`,
				`{"result": {"data": "0x123"}}`,
				`{"result": {"data": "0x123", "logs": null}}`,
			},
		},
		{
			name: "null values vs non-existing fields",
			variants: []string{
				`{"result": {"from": "0xabc", "to": null}}`,
				`{"result": {"from": "0xabc"}}`,
				`{"result": {"from": "0xabc", "to": ""}}`,
			},
		},
		{
			name: "empty strings and hex values",
			variants: []string{
				`{"result": {"data": "", "value": "0x123"}}`,
				`{"result": {"data": "0x", "value": "0x123"}}`,
				`{"result": {"data": "0x0", "value": "0x123"}}`,
				`{"result": {"value": "0x123"}}`,
			},
		},
		{
			name: "nested empty objects",
			variants: []string{
				`{"result": {"block": {"transactions": []}, "status": "ok"}}`,
				`{"result": {"block": {}, "status": "ok"}}`,
				`{"result": {"status": "ok"}}`,
			},
		},
		{
			name: "field ordering should not matter",
			variants: []string{
				`{"result": {"a": "1", "b": "2", "c": "3"}}`,
				`{"result": {"c": "3", "a": "1", "b": "2"}}`,
				`{"result": {"b": "2", "c": "3", "a": "1"}}`,
			},
		},
		{
			name: "real-world example - transaction fields with nulls",
			variants: []string{
				`{"result": {"hash": "0xabc", "from": "0x123", "to": "0x456", "accessList": [], "chainId": null, "yParity": null}}`,
				`{"result": {"hash": "0xabc", "from": "0x123", "to": "0x456"}}`,
				`{"result": {"from": "0x123", "hash": "0xabc", "to": "0x456", "accessList": null}}`,
			},
		},
		{
			name: "array with empty elements",
			variants: []string{
				`{"result": {"items": ["a", "", "c"]}}`,
				`{"result": {"items": ["a", null, "c"]}}`,
				`{"result": {"items": ["a", "0x", "c"]}}`,
			},
		},
		{
			name: "hex values with leading zeros",
			variants: []string{
				`{"result": {
                "baseFeePerGas": "0x989680",
                "difficulty": "0x1",
                "extraData": "0xc55914e90e13cba05a84efc4b7f1e7c44897f9437ed4fbac55ae8d602d4d3240",
                "gasLimit": "0x4000000000000",
                "gasUsed": "0x93349",
                "hash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                "l1BlockNumber": "0x15b88c4",
                "logsBloom": "0x00000000000000000000000000000000000000008008000000080000001000000000000000000008000000000000000000004000000020000000040000208000080010010200000800000008000100000000000000040000000000000000000000004000000000000000000400000000000010000000400000400010000800000000000000000000000c00000000000000000000000000000000080000000000022000000000000000000000000c00000000102040000000000008000000000000008002000000000000000200000000008000000400000000000000000000008010000000000000000000000000000210000000000000000000000000000000",
                "miner": "0xa4b000000000000000000073657175656e636572",
                "mixHash": "0x0000000000023fb900000000015b88c400000000000000280000000000000000",
                "nonce": "0x00000000001ed08c",
                "number": "0x14e8a59a",
                "parentHash": "0xeea5e5abd449dcc96b06dc7fcf1d11e19b6ed31d1c568f83e21e4e80b6e97db2",
                "receiptsRoot": "0xbf6cfdf29ce73f87c2c0b828813e5700a5fa621777bbbae650c8b34cdaa8b15b",
                "sendCount": "0x23fb9",
                "sendRoot": "0xc55914e90e13cba05a84efc4b7f1e7c44897f9437ed4fbac55ae8d602d4d3240",
                "sha3Uncles": "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
                "size": "0x4c0",
                "stateRoot": "0xe8d2484861442a3ef9809818951b22776d6ca444d7e31f99c0c104532be4f690",
                "timestamp": "0x685aeaf0",
                "transactions": [
                    {
                        "blockHash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                        "blockNumber": "0x14e8a59a",
                        "from": "0x00000000000000000000000000000000000a4b05",
                        "gas": "0x0",
                        "gasPrice": "0x0",
                        "hash": "0x03dd259cba9ef03a0043700ff6aa27de5c96337ad410b3710b6482ca8a971fd0",
                        "input": "0x6bf6a42d000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000015b88c40000000000000000000000000000000000000000000000000000000014e8a59a0000000000000000000000000000000000000000000000000000000000000000",
                        "nonce": "0x0",
                        "to": "0x00000000000000000000000000000000000a4b05",
                        "transactionIndex": "0x0",
                        "value": "0x0",
                        "test": "0x1",
                        "type": "0x6a",
                        "chainId": "0xa4b1",
                        "v": "0x0",
                        "r": "0x0",
                        "s": "0x0"
                    },
                    {
                        "blockHash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                        "blockNumber": "0x14e8a59a",
                        "from": "0x975227fccc2e9622c337b154fbca1f610f8b980b",
                        "gas": "0x5c521",
                        "gasPrice": "0xe4e1c0",
                        "maxFeePerGas": "0xe4e1c0",
                        "maxPriorityFeePerGas": "0xe4e1c0",
                        "hash": "0x4bbf17269f52fc3f5b08d4361c78547dc41cfe851a1cee8ce8d8183529b08e20",
                        "input": "0xb2460c480013000c07c6fa82dbcc0000000000000a05686d372518312e83fe47c0eae100",
                        "nonce": "0x7864c",
                        "to": "0xcb43d843f6cadf4f4844f3f57032468aadd9b95c",
                        "transactionIndex": "0x1",
                        "value": "0x0",
                        "type": "0x2",
                        "accessList": [],
                        "chainId": "0xa4b1",
                        "v": "0x0",
                        "r": "0x1d899e5a4bba036c903793973bf05d5558ba74390bb1fbf3debffaa81955974",
                        "s": "0x52023c741692c2b90fb2badfd85beb7a234c91815efd90e6787d73af836a7a7d",
                        "yParity": "0x0"
                    },
                    {
                        "blockHash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                        "blockNumber": "0x14e8a59a",
                        "from": "0x4b260f8879b49365342f44a2b269defa67066a91",
                        "gas": "0x32deb",
                        "gasPrice": "0x989680",
                        "maxFeePerGas": "0x1312d00",
                        "maxPriorityFeePerGas": "0x0",
                        "hash": "0x29905570a812fcd1b87b1aa5ac1965b165e1ab62aa43cb19d8b2bff6cc9e61b5",
                        "input": "0xa9059cbb000000000000000000000000d04c57f232e21371d5502bc5f79033b9045e9f1a000000000000000000000000000000000000000000000000002386f26fc10000",
                        "nonce": "0xd3d4",
                        "to": "0x912ce59144191c1204e64559fe8253a0e49e6548",
                        "transactionIndex": "0x2",
                        "value": "0x0",
                        "type": "0x2",
                        "accessList": [],
                        "chainId": "0xa4b1",
                        "v": "0x1",
                        "r": "0xe45fa22c58004d242c057f06470ed992032f5c8adede2e700c08af1200ffc9dc",
                        "s": "0x673e40d58c4e1b3fafa7bc6e014d0ae3c20ae92827d4c86dc8fae5c4c61fed1b",
                        "yParity": "0x1"
                    },
                    {
                        "blockHash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                        "blockNumber": "0x14e8a59a",
                        "from": "0xf89d7b9c864f589bbf53a82105107622b35eaa40",
                        "gas": "0x2dc6c0",
                        "gasPrice": "0x1312d00",
                        "maxFeePerGas": "0xb2d05e00",
                        "maxPriorityFeePerGas": "0x989680",
                        "hash": "0x687b2b8c3ae04b2d658072cdecbc507abf120a1de1d3d314e4e4668478fa1b15",
                        "input": "0xa9059cbb0000000000000000000000008b72bffc0704adfbd54aacff75c7786b34de5090000000000000000000000000000000000000000000000000000000000ca2dd00",
                        "nonce": "0x47a57c",
                        "to": "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9",
                        "transactionIndex": "0x3",
                        "value": "0x0",
                        "type": "0x2",
                        "accessList": [],
                        "chainId": "0xa4b1",
                        "v": "0x1",
                        "r": "0xb4e0f23588a9ec8ba28eac6fb5d5730fbbb353347a73bf7516c4f98ed65c26de",
                        "s": "0xcb85359e91835eb3440bcef930ee40f7e96e9e0ab4aebec5f30554ca862e9b9",
                        "yParity": "0x1"
                    }
                ],
                "transactionsRoot": "0xec2be67d104f0e1947f0ff32c22c2c0b5fe83d21aa87da153723807d23e32644",
                "uncles": []
            }}`,
				`{"result": {
                "hash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                "timestamp": "0x685aeaf0",
                "baseFeePerGas": "0x989680",
                "l1BlockNumber": "0x15b88c4",
                "sendCount": "0x23fb9",
                "mixHash": "0x0000000000023fb900000000015b88c400000000000000280000000000000000",
                "number": "0x14e8a59a",
                "logsBloom": "0x00000000000000000000000000000000000000008008000000080000001000000000000000000008000000000000000000004000000020000000040000208000080010010200000800000008000100000000000000040000000000000000000000004000000000000000000400000000000010000000400000400010000800000000000000000000000c00000000000000000000000000000000080000000000022000000000000000000000000c00000000102040000000000008000000000000008002000000000000000200000000008000000400000000000000000000008010000000000000000000000000000210000000000000000000000000000000",
                "miner": "0xa4b000000000000000000073657175656e636572",
                "extraData": "0xc55914e90e13cba05a84efc4b7f1e7c44897f9437ed4fbac55ae8d602d4d3240",
                "gasUsed": "0x93349",
                "nonce": "0x00000000001ed08c",
                "difficulty": "0x1",
                "uncles": [],
                "sha3Uncles": "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
                "transactionsRoot": "0xec2be67d104f0e1947f0ff32c22c2c0b5fe83d21aa87da153723807d23e32644",
                "receiptsRoot": "0xbf6cfdf29ce73f87c2c0b828813e5700a5fa621777bbbae650c8b34cdaa8b15b",
                "transactions": [
                    {
                        "hash": "0x03dd259cba9ef03a0043700ff6aa27de5c96337ad410b3710b6482ca8a971fd0",
                        "type": "0x6a",
                        "blockHash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                        "gasPrice": "0x0",
                        "r": "0x00",
                        "s": "0x00",
                        "accessList": [],
                        "nonce": "0x0",
                        "from": "0x00000000000000000000000000000000000a4b05",
                        "to": "0x00000000000000000000000000000000000a4b05",
                        "transactionIndex": "0x0",
                        "gas": "0x0",
                        "value": "0x0",
                        "test": "0x1",
                        "yParity": null,
                        "input": "0x6bf6a42d000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000015b88c40000000000000000000000000000000000000000000000000000000014e8a59a0000000000000000000000000000000000000000000000000000000000000000",
                        "blockNumber": "0x14e8a59a",
                        "v": "0x00",
                        "chainId": "0xa4b1",
                        "l1Fee": null
                    },
                    {
                        "from": "0x975227fccc2e9622c337b154fbca1f610f8b980b",
                        "type": "0x2",
                        "to": "0xcb43d843f6cadf4f4844f3f57032468aadd9b95c",
                        "blockHash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                        "v": "0x00",
                        "yParity": "0x0",
                        "gas": "0x5c521",
                        "value": "0x0",
                        "maxFeePerGas": "0xe4e1c0",
                        "maxPriorityFeePerGas": "0xe4e1c0",
                        "chainId": "0xa4b1",
                        "input": "0xb2460c480013000c07c6fa82dbcc0000000000000a05686d372518312e83fe47c0eae100",
                        "blockNumber": "0x14e8a59a",
                        "gasPrice": "0xe4e1c0",
                        "r": "0x01d899e5a4bba036c903793973bf05d5558ba74390bb1fbf3debffaa81955974",
                        "s": "0x52023c741692c2b90fb2badfd85beb7a234c91815efd90e6787d73af836a7a7d",
                        "l1Fee": null,
                        "hash": "0x4bbf17269f52fc3f5b08d4361c78547dc41cfe851a1cee8ce8d8183529b08e20",
                        "nonce": "0x7864c",
                        "transactionIndex": "0x1",
                        "accessList": []
                    },
                    {
                        "transactionIndex": "0x2",
                        "maxFeePerGas": "0x1312d00",
                        "maxPriorityFeePerGas": "0x0",
                        "s": "0x673e40d58c4e1b3fafa7bc6e014d0ae3c20ae92827d4c86dc8fae5c4c61fed1b",
                        "chainId": "0xa4b1",
                        "accessList": [],
                        "hash": "0x29905570a812fcd1b87b1aa5ac1965b165e1ab62aa43cb19d8b2bff6cc9e61b5",
                        "from": "0x4b260f8879b49365342f44a2b269defa67066a91",
                        "l1Fee": null,
                        "gasPrice": "0x989680",
                        "v": "0x01",
                        "to": "0x912ce59144191c1204e64559fe8253a0e49e6548",
                        "blockNumber": "0x14e8a59a",
                        "value": "0x0",
                        "r": "0xe45fa22c58004d242c057f06470ed992032f5c8adede2e700c08af1200ffc9dc",
                        "nonce": "0xd3d4",
                        "type": "0x2",
                        "blockHash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                        "yParity": "0x1",
                        "gas": "0x32deb",
                        "input": "0xa9059cbb000000000000000000000000d04c57f232e21371d5502bc5f79033b9045e9f1a000000000000000000000000000000000000000000000000002386f26fc10000"
                    },
                    {
                        "type": "0x2",
                        "blockHash": "0x095e8f52e77f0add52fc6cf2f3f04ceb72462dbf54bab11544e7227415aeabd5",
                        "transactionIndex": "0x3",
                        "maxPriorityFeePerGas": "0x989680",
                        "r": "0xb4e0f23588a9ec8ba28eac6fb5d5730fbbb353347a73bf7516c4f98ed65c26de",
                        "v": "0x01",
                        "nonce": "0x47a57c",
                        "from": "0xf89d7b9c864f589bbf53a82105107622b35eaa40",
                        "chainId": "0xa4b1",
                        "yParity": "0x1",
                        "l1Fee": null,
                        "to": "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9",
                        "blockNumber": "0x14e8a59a",
                        "value": "0x0",
                        "accessList": [],
                        "hash": "0x687b2b8c3ae04b2d658072cdecbc507abf120a1de1d3d314e4e4668478fa1b15",
                        "gas": "0x2dc6c0",
                        "maxFeePerGas": "0xb2d05e00",
                        "s": "0x0cb85359e91835eb3440bcef930ee40f7e96e9e0ab4aebec5f30554ca862e9b9",
                        "input": "0xa9059cbb0000000000000000000000008b72bffc0704adfbd54aacff75c7786b34de5090000000000000000000000000000000000000000000000000000000000ca2dd00",
                        "gasPrice": "0x1312d00"
                    }
                ],
                "parentHash": "0xeea5e5abd449dcc96b06dc7fcf1d11e19b6ed31d1c568f83e21e4e80b6e97db2",
                "stateRoot": "0xe8d2484861442a3ef9809818951b22776d6ca444d7e31f99c0c104532be4f690",
                "size": "0x4c0",
                "gasLimit": "0x4000000000000",
                "totalDifficulty": "0x0",
                "sendRoot": "0xc55914e90e13cba05a84efc4b7f1e7c44897f9437ed4fbac55ae8d602d4d3240"
            }}`,
			},
		},
		{
			name: "missing values and 0x0/false to be equal",
			variants: []string{
				`{
                "blockHash": "0x022adc8dcc3292aab691774ac5a359549ba438acc1ac1be439db8cd3b345102d",
                "blockNumber": "0x14ea213c",
                "contractAddress": null,
                "cumulativeGasUsed": "0x0",
                "effectiveGasPrice": "0x989680",
                "from": "0x00000000000000000000000000000000000a4b05",
                "gasUsed": "0x0",
                "gasUsedForL1": "0x0",
                "logs": [],
                "logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
                "status": "0x1",
				"l1BlockNumber": "0x000",
                "to": "0x00000000000000000000000000000000000a4b05",
                "transactionHash": "0x932b9262bcb303c215e4647e5dc6e9bebdd03cd0ef9e75281957bf39c492ecff",
                "transactionIndex": "0x0",
                "type": "0x6a"
            }`,
				`{
                "cumulativeGasUsed": "0x0",
                "to": "0x00000000000000000000000000000000000a4b05",
                "status": "0x1",
                "type": "0x6a",
                "from": "0x00000000000000000000000000000000000a4b05",
                "logs": [],
                "l1Fee": null,
                "effectiveGasPrice": "0x989680",
                "transactionHash": "0x932b9262bcb303c215e4647e5dc6e9bebdd03cd0ef9e75281957bf39c492ecff",
                "blockHash": "0x022adc8dcc3292aab691774ac5a359549ba438acc1ac1be439db8cd3b345102d",
                "blockNumber": "0x14ea213c",
                "gasUsed": "0x0",
                "logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
                "contractAddress": null,
                "transactionIndex": "0x0",
                "l1GasUsed": null,
                "l1GasPrice": null
            }`,
			},
		},
	}

	for _, group := range testGroups {
		group := group
		t.Run(group.name, func(t *testing.T) {
			t.Parallel()

			var hashes []string
			for i, variant := range group.variants {
				resp := &JsonRpcResponse{
					Result: []byte(variant),
				}
				hash, err := resp.CanonicalHash()
				assert.NoError(t, err, "variant %d: %s", i, variant)
				hashes = append(hashes, hash)
			}

			// All variants in the group should produce the same hash
			for i := 1; i < len(hashes); i++ {
				assert.Equal(t, hashes[0], hashes[i],
					"Hash mismatch between variant 0 and variant %d\nVariant 0: %s\nVariant %d: %s",
					i, group.variants[0], i, group.variants[i])
			}
		})
	}

	// Test cases that should produce different hashes
	t.Run("different values should produce different hashes", func(t *testing.T) {
		cases := []string{
			`{"result": {"value": "0x123"}}`,
			// `{"result": {"value": "123"}}`,
			`{"result": {"value": "0x456"}}`,
			`{"result": {"data": "0x123"}}`,
		}

		hashes := make(map[string]bool)
		for _, c := range cases {
			resp := &JsonRpcResponse{Result: []byte(c)}
			hash, err := resp.CanonicalHash()
			assert.NoError(t, err)
			assert.False(t, hashes[hash], "Duplicate hash found for: %s", c)
			hashes[hash] = true
		}
		assert.Equal(t, len(cases), len(hashes), "Expected all different hashes")
	})

	// Test the specific shadow response example
	t.Run("shadow response normalization", func(t *testing.T) {
		// Simplified version of the shadow response example
		original := `{
			"result": {
				"hash": "0xa52b",
				"transactions": [{
					"hash": "0x18c4",
					"from": "0x3d2f",
					"chainId": null,
					"accessList": []
				}]
			}
		}`

		shadow := `{
			"result": {
				"transactions": [{
					"from": "0x3d2f",
					"hash": "0x18c4"
				}],
				"hash": "0xa52b"
			}
		}`

		resp1 := &JsonRpcResponse{Result: []byte(original)}
		resp2 := &JsonRpcResponse{Result: []byte(shadow)}

		hash1, err1 := resp1.CanonicalHash()
		hash2, err2 := resp2.CanonicalHash()

		assert.NoError(t, err1)
		assert.NoError(t, err2)
		assert.Equal(t, hash1, hash2, "Shadow response should have same canonical hash")
	})
}

// Append this test at the end of the file
func TestJsonRpcRequest_CloneDeepCopy(t *testing.T) {
	// Test that Clone creates a deep copy of Params to avoid concurrent access issues
	original := &JsonRpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_getLogs",
		Params: []interface{}{
			map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x2",
				"address":   []interface{}{"0xabc", "0xdef"},
				"topics":    []interface{}{"0x123", "0x456"},
			},
		},
	}

	// Clone the request
	cloned := original.Clone()

	// Verify basic fields are copied
	assert.Equal(t, original.JSONRPC, cloned.JSONRPC)
	assert.Equal(t, original.Method, cloned.Method)
	assert.Equal(t, original.ID, cloned.ID) // ID is copied as-is

	// Verify Params is deep copied
	assert.Equal(t, len(original.Params), len(cloned.Params))

	// Modify the cloned map
	clonedMap := cloned.Params[0].(map[string]interface{})
	clonedMap["fromBlock"] = "0x99"
	clonedMap["newField"] = "test"

	// Modify the address array in the cloned map
	clonedAddresses := clonedMap["address"].([]interface{})
	clonedAddresses[0] = "0xmodified"

	// Verify original is unchanged
	originalMap := original.Params[0].(map[string]interface{})
	assert.Equal(t, "0x1", originalMap["fromBlock"])
	assert.Nil(t, originalMap["newField"])

	originalAddresses := originalMap["address"].([]interface{})
	assert.Equal(t, "0xabc", originalAddresses[0])

	// Test concurrent access safety
	t.Run("ConcurrentAccess", func(t *testing.T) {
		const goroutines = 100
		var wg sync.WaitGroup
		wg.Add(goroutines)

		for i := 0; i < goroutines; i++ {
			go func(idx int) {
				defer wg.Done()

				// Clone the request
				clone := original.Clone()

				// Access and modify the cloned params
				if m, ok := clone.Params[0].(map[string]interface{}); ok {
					// Read
					_ = m["fromBlock"]
					_ = m["toBlock"]

					// Write
					m[fmt.Sprintf("field_%d", idx)] = idx
				}
			}(i)
		}

		wg.Wait()

		// Verify original is still unchanged
		originalMap := original.Params[0].(map[string]interface{})
		assert.Equal(t, 4, len(originalMap)) // Should still have only original 4 fields
		assert.Equal(t, "0x1", originalMap["fromBlock"])
		assert.Equal(t, "0x2", originalMap["toBlock"])
	})
}
