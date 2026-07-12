package sdk

import "gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"

type knowledgeImpl struct{ ks *knowledge.Store }

func NewKnowledge(ks *knowledge.Store) KnowledgeAPI { return &knowledgeImpl{ks: ks} }

func (k *knowledgeImpl) Search(query string, topK int) ([]*Knowledge, error) {
	if k.ks == nil { return nil, nil }
	got := k.ks.Search(query, topK)
	out := make([]*Knowledge, len(got))
	for i, item := range got {
		out[i] = &Knowledge{Name: item.Name, Content: item.Content}
	}
	return out, nil
}

func (k *knowledgeImpl) Add(name, content string) error {
	if k.ks == nil { return nil }
	return k.ks.Add(name, content)
}

func (k *knowledgeImpl) List() ([]string, error) {
	if k.ks == nil { return nil, nil }
	return k.ks.List(), nil
}

var _ KnowledgeAPI = (*knowledgeImpl)(nil)
