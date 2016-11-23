package influxql

type iteratorMapper struct {
	e         *Emitter
	fields    []int // which iterator to use for an aux field
	auxFields []interface{}
}

func NewIteratorMapper(itrs []Iterator, fields []int, opt IteratorOptions) Iterator {
	e := NewEmitter(itrs, opt.Ascending, 0)
	e.OmitTime = true
	return &iteratorMapper{
		e:         e,
		fields:    fields,
		auxFields: make([]interface{}, len(fields)),
	}
}

func (itr *iteratorMapper) Next() (*FloatPoint, error) {
	t, name, tags, err := itr.e.loadBuf()
	if err != nil || t == ZeroTime {
		return nil, err
	}

	aux := itr.e.readAt(t, name, tags)
	for i, f := range itr.fields {
		itr.auxFields[i] = aux[f]
	}
	return &FloatPoint{
		Name: name,
		Tags: tags,
		Time: t,
		Aux:  itr.auxFields,
	}, nil
}

func (itr *iteratorMapper) Stats() IteratorStats {
	stats := IteratorStats{}
	for _, itr := range itr.e.itrs {
		stats.Add(itr.Stats())
	}
	return stats
}

func (itr *iteratorMapper) Close() error {
	return itr.e.Close()
}
