"""生成侧图的定义（供 export_qwen3vl_embedding_onnx.py 复用）。

为什么单独一张图，而不是改 Transformer.onnx
------------------------------------------
Transformer.onnx 的输出是 `embedding` = 最后 token 经 last_token 池化后的
[dim]，而生成需要**每一步的全序列 hidden**。两种输出形状不同，塞进一张图
只能靠 dynamic_axes 变通，而导出脚本已经因为「签名写着 dynamic 却只能用导出
长度跑」吃过亏（见文件头注释）。

所以新增一张图，**TokenEmbedding.onnx 与 Transformer 的权重分片直接复用**
——onnxruntime 的 external data 是按文件名的，共享权重要靠 onnx 层面；
因此这里选择最省事的形态：生成侧图自己带权重（external data 分片独立），
代价是磁盘多一份 hidden 侧的 4GB。**但 TokenEmbedding 仍共用**，
因为 tied head 的权重就在那张图里。

tied lm_head：零新增权重
----------------------
Qwen3-VL 的 `tie_word_embeddings=True`，实测 safetensors 里 625 个张量
**没有独立 lm_head**。输出层就是 `embed_tokens.weight` 转置：

    logits = normed_hidden @ embed_tokens.weight.T

所以 lm_head 不需要任何新权重 —— embed_tokens 已在 TokenEmbedding.onnx 里。
本模块把 embed_tokens 作为常量折进图里（external data），运行时无需再算。

KV cache：不做
--------------
自回归解码若每步全量前向是 O(N²)。但拆分任务的输出极短（实测
3~7 行 × 10~20 token，N≈200），且**蒸馏是 30 分钟一次的低频任务**，
不是检索热路径。为它引入 KV cache 状态机（position 偏移、past 拼接、
attention mask 增量构造）会让导出脚本复杂度翻倍，而收益只在这条低频路径上。

若将来生成侧进了热路径，再补 KV cache —— 那时应该先量 O(N²) 的实际延迟。
"""

import torch


class LMHead(torch.nn.Module):
    """全序列 hidden → logits（tied head，不含 lm_head 权重拷贝）。

    lm_head 权重折进来成为常量，于是这张图是自包含的（只需 hidden 输入）。
    vocab 151936 × dim 2048 的权重以 external data 落盘，与 TokenEmbedding
    里的那份内容相同但各存一份 —— onnxruntime 不支持跨文件共享 external data。
    """

    def __init__(self, lm):
        super().__init__()
        # tie_word_embeddings=True ⇒ lm_head 就是 embed_tokens 转置。
        self.weight = lm.embed_tokens.weight

    def forward(self, hidden):
        # [1, seq, dim] @ [vocab, dim].T → [1, seq, vocab]
        return torch.matmul(hidden, self.weight.t())


class HiddenOnly(torch.nn.Module):
    """Transformer 全序列版：只跑层与 final norm，**不池化**。

    与导出脚本里 TransformerWrapper 的差别只有一处：返回 `norm(hidden)`
    而不是 `norm(hidden)[:, -1]`。层权重、DeepStack 相加、RoPE 全一致，
    所以同一份输入下，两张图在最后一个位置上给出相同的向量。

    这是「一份权重两个模式」的核心：embedding 路径走池化版（省算力），
    生成路径走全序列版（拿得到每步 hidden）。
    """

    def __init__(self, lm):
        super().__init__()
        self.layers = lm.layers
        self.norm = lm.norm

    def forward(self, hidden, deepstack_0, deepstack_1, deepstack_2,
                rotary_cos, rotary_sin, causal_mask):
        deep = (deepstack_0, deepstack_1, deepstack_2)
        for i, layer in enumerate(self.layers):
            hidden = layer(
                hidden_states=hidden,
                attention_mask=causal_mask,
                position_embeddings=(rotary_cos, rotary_sin),
                use_cache=False,
            )
            if i < 3:
                hidden = hidden + deep[i]
        return self.norm(hidden)


class SequenceWithHead(torch.nn.Module):
    """全序列 hidden + tied head → logits（生成侧的单张图）。

    合成 HiddenOnly 与 LMHead：省一次 host↔device 往返，也让「这张图的
    输出就是 logits」这件事在签名上自明。
    """

    def __init__(self, lm):
        super().__init__()
        self.trunk = HiddenOnly(lm)
        self.head = LMHead(lm)

    def forward(self, hidden, deepstack_0, deepstack_1, deepstack_2,
                rotary_cos, rotary_sin, causal_mask):
        h = self.trunk(hidden, deepstack_0, deepstack_1, deepstack_2,
                       rotary_cos, rotary_sin, causal_mask)
        return self.head(h)
