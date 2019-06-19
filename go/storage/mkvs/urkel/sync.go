package urkel

import (
	"context"

	"github.com/pkg/errors"

	"github.com/oasislabs/ekiden/go/storage/mkvs/urkel/node"
	"github.com/oasislabs/ekiden/go/storage/mkvs/urkel/syncer"
)

var _ syncer.ReadSyncer = (*Tree)(nil)

// GetSubtree retrieves a compressed subtree summary of the given node
// under the given root up to the specified depth.
//
// It is the responsibility of the caller to validate that the subtree
// is correct and consistent.
func (t *Tree) GetSubtree(ctx context.Context, root node.Root, id node.ID, maxDepth node.Depth) (*syncer.Subtree, error) {
	t.cache.Lock()
	defer t.cache.Unlock()

	if !root.Equal(&t.cache.syncRoot) {
		return nil, syncer.ErrInvalidRoot
	}
	if !t.cache.pendingRoot.IsClean() {
		return nil, syncer.ErrDirtyRoot
	}

	// Extract the node that is at the root of the subtree.
	subtreeRoot, bd, err := t.cache.derefNodeID(ctx, id)
	if err != nil {
		return nil, syncer.ErrNodeNotFound
	}

	// path corresponds to already navigated prefix of the key up to bd bits.
	path, _ := id.Path.Split(bd, id.Path.BitLength())

	st := &syncer.Subtree{}
	right := false
	if len(id.Path) > 0 {
		right = id.Path.GetBit(bd)
	}
	rootPtr, err := t.doGetSubtree(ctx, subtreeRoot, bd, path, st, 0, maxDepth, right)
	if err != nil {
		return nil, errors.Wrap(err, "urkel: failed to get subtree")
	}
	st.Root = rootPtr
	if !st.Root.Valid {
		return nil, syncer.ErrInvalidRoot
	}

	return st, nil
}

func (t *Tree) doGetSubtree(
	ctx context.Context,
	ptr *node.Pointer,
	bitDepth node.Depth,
	path node.Key,
	st *syncer.Subtree,
	depth node.Depth,
	maxDepth node.Depth,
	right bool,
) (syncer.SubtreePointer, error) {
	// Abort in case the context is cancelled.
	select {
	case <-ctx.Done():
		return syncer.SubtreePointer{}, ctx.Err()
	default:
	}

	nd, err := t.cache.derefNodePtr(ctx, node.ID{Path: path.AppendBit(bitDepth, right), BitDepth: bitDepth + 1}, ptr, nil)
	if err != nil {
		return syncer.SubtreePointer{}, err
	}
	if nd == nil {
		return syncer.SubtreePointer{Index: syncer.InvalidSubtreeIndex, Valid: true}, nil
	}

	if depth >= maxDepth {
		// Nodes at maxDepth are always full nodes.
		idx, err := st.AddFullNode(nd.Extract())
		if err != nil {
			return syncer.SubtreePointer{}, err
		}
		return syncer.SubtreePointer{Index: idx, Full: true, Valid: true}, nil
	}

	switch n := nd.(type) {
	case *node.InternalNode:
		// Record internal node summary.
		s := syncer.InternalNodeSummary{}

		// To traverse subtrees resize path bit vector, if needed.
		path = path.Merge(bitDepth, n.Label, n.LabelBitLength)

		s.Label = n.Label
		s.LabelBitLength = n.LabelBitLength

		newPath := path.Merge(bitDepth, n.Label, n.LabelBitLength)
		// Leaf node.
		leafNodePtr, err := t.doGetSubtree(ctx, n.LeafNode, bitDepth+n.LabelBitLength, newPath, st, depth, maxDepth, false)
		if err != nil {
			return syncer.SubtreePointer{}, err
		}
		s.LeafNode = leafNodePtr

		// Left subtree.
		leftPtr, err := t.doGetSubtree(ctx, n.Left, bitDepth+n.LabelBitLength, newPath, st, depth+1, maxDepth, false)
		if err != nil {
			return syncer.SubtreePointer{}, err
		}
		s.Left = leftPtr

		// Right subtree.
		rightPtr, err := t.doGetSubtree(ctx, n.Right, bitDepth+n.LabelBitLength, newPath, st, depth+1, maxDepth, true)
		if err != nil {
			return syncer.SubtreePointer{}, err
		}
		s.Right = rightPtr

		idx, err := st.AddSummary(s)
		if err != nil {
			return syncer.SubtreePointer{}, err
		}

		return syncer.SubtreePointer{Index: idx, Valid: true}, nil
	case *node.LeafNode:
		// All encountered leaves are always full nodes.
		idx, err := st.AddFullNode(nd.Extract())
		if err != nil {
			return syncer.SubtreePointer{}, err
		}

		return syncer.SubtreePointer{Index: idx, Full: true, Valid: true}, nil
	default:
		panic("urkel: invalid node type")
	}
}

// GetPath retrieves a compressed path summary for the given key under
// the given root, starting at the given bit depth.
//
// It is the responsibility of the caller to validate that the subtree
// is correct and consistent.
func (t *Tree) GetPath(ctx context.Context, root node.Root, key node.Key, startBitDepth node.Depth) (*syncer.Subtree, error) {
	t.cache.Lock()
	defer t.cache.Unlock()

	if !root.Equal(&t.cache.syncRoot) {
		return nil, syncer.ErrInvalidRoot
	}
	if !t.cache.pendingRoot.IsClean() {
		return nil, syncer.ErrDirtyRoot
	}

	subtreeRoot, bd, err := t.cache.derefNodeID(ctx, node.ID{Path: key, BitDepth: startBitDepth})
	if err != nil {
		return nil, syncer.ErrNodeNotFound
	}

	st := &syncer.Subtree{}
	right := false
	if len(key) > 0 {
		right = key.GetBit(bd)
	}

	// path corresponds to already navigated prefix of the key up to bd bits.
	path, _ := key.Split(bd, key.BitLength())
	rootPtr, err := t.doGetPath(ctx, subtreeRoot, bd, path, &key, st, right)
	if err != nil {
		return nil, errors.Wrap(err, "urkel: failed to get path")
	}
	st.Root = rootPtr
	if !st.Root.Valid {
		return nil, syncer.ErrInvalidRoot
	}

	return st, nil
}

func (t *Tree) doGetPath(
	ctx context.Context,
	ptr *node.Pointer,
	bitDepth node.Depth,
	path node.Key,
	key *node.Key,
	st *syncer.Subtree,
	right bool,
) (syncer.SubtreePointer, error) {
	// Abort in case the context is cancelled.
	select {
	case <-ctx.Done():
		return syncer.SubtreePointer{}, ctx.Err()
	default:
	}

	extPath := path.AppendBit(bitDepth, right)
	nd, err := t.cache.derefNodePtr(ctx, node.ID{Path: extPath, BitDepth: bitDepth + 1}, ptr, extPath)
	if err != nil {
		return syncer.SubtreePointer{}, err
	}
	if nd == nil {
		return syncer.SubtreePointer{Index: syncer.InvalidSubtreeIndex, Valid: true}, nil
	}

	if key == nil {
		// Off-path nodes are always full nodes.
		idx, err := st.AddFullNode(nd.Extract())
		if err != nil {
			return syncer.SubtreePointer{}, err
		}
		return syncer.SubtreePointer{Index: idx, Full: true, Valid: true}, nil
	}

	switch n := nd.(type) {
	case *node.InternalNode:
		// Record internal node summary.
		s := syncer.InternalNodeSummary{}
		// Determine which subtree is off-path.
		var leftKey, rightKey *node.Key
		if bitDepth+n.LabelBitLength < key.BitLength() {
			if key.GetBit(bitDepth + n.LabelBitLength) {
				// Left subtree is off-path.
				rightKey = key
			} else {
				// Right subtree is off-path.
				leftKey = key
			}
		}

		s.Label = n.Label
		s.LabelBitLength = n.LabelBitLength

		newPath := path.Merge(bitDepth, n.Label, n.LabelBitLength)
		// Leaf node.
		leafNodePtr, err := t.doGetPath(ctx, n.LeafNode, bitDepth+n.LabelBitLength, newPath, &newPath, st, false)
		if err != nil {
			return syncer.SubtreePointer{}, err
		}
		s.LeafNode = leafNodePtr

		// Left subtree.
		leftPtr, err := t.doGetPath(ctx, n.Left, bitDepth+n.LabelBitLength, newPath, leftKey, st, false)
		if err != nil {
			return syncer.SubtreePointer{}, err
		}
		s.Left = leftPtr

		// Right subtree.
		rightPtr, err := t.doGetPath(ctx, n.Right, bitDepth+n.LabelBitLength, newPath, rightKey, st, true)
		if err != nil {
			return syncer.SubtreePointer{}, err
		}
		s.Right = rightPtr

		idx, err := st.AddSummary(s)
		if err != nil {
			return syncer.SubtreePointer{}, err
		}

		return syncer.SubtreePointer{Index: idx, Full: false, Valid: true}, nil
	case *node.LeafNode:
		// All encountered leaves are always full nodes.
		idx, err := st.AddFullNode(nd.Extract())
		if err != nil {
			return syncer.SubtreePointer{}, err
		}

		return syncer.SubtreePointer{Index: idx, Full: true, Valid: true}, nil
	default:
		panic("urkel: invalid node type")
	}
}

// GetNode retrieves a specific node under the given root.
//
// It is the responsibility of the caller to validate that the node
// is consistent. The node's cached hash should be considered invalid
// and must be recomputed locally.
func (t *Tree) GetNode(ctx context.Context, root node.Root, id node.ID) (node.Node, error) {
	t.cache.Lock()
	defer t.cache.Unlock()

	if !root.Equal(&t.cache.syncRoot) {
		return nil, syncer.ErrInvalidRoot
	}
	if !t.cache.pendingRoot.IsClean() {
		return nil, syncer.ErrDirtyRoot
	}
	ptr, _, err := t.cache.derefNodeID(ctx, id)
	if err != nil {
		return nil, syncer.ErrNodeNotFound
	}
	nd, err := t.cache.derefNodePtr(ctx, id, ptr, nil)
	if err != nil {
		return nil, syncer.ErrNodeNotFound
	}

	return nd.Extract(), nil
}
