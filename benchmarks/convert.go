package benchmarks

import "github.com/oarkflow/codec/benchmarks/pb"

func (d *Doc) ToPB() *pb.Doc {
	o := &pb.Doc{Id: d.ID, Name: d.Name, Age: d.Age, Active: d.Active, Score: d.Score, Ratio: d.Ratio,
		Delta: d.Delta, Tags: d.Tags, Times: d.Times, Small: d.Small, Raw: d.Raw, Flags: d.Flags, Fs: d.Fs,
		Home: &pb.Addr{Street: d.Home.Street, Zip: d.Home.Zip}}
	if d.Addr != nil {
		o.Addr = &pb.Addr{Street: d.Addr.Street, Zip: d.Addr.Zip}
	}
	for _, it := range d.Items {
		o.Items = append(o.Items, &pb.Item{Sku: it.SKU, Qty: it.Qty, Price: it.Price})
	}
	return o
}

func (b *Batch) ToPB() *pb.Batch {
	o := &pb.Batch{Source: b.Source, Points: make([]*pb.Point, len(b.Points))}
	for i, p := range b.Points {
		o.Points[i] = &pb.Point{Ts: p.TS, Host: p.Host, Val: p.Val, Status: p.Status}
	}
	return o
}
