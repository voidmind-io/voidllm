package db

// queryArgs accumulates positional query arguments and hands out the matching
// dialect placeholder for each one. Arguments must be bound in the textual
// order in which their placeholders appear in the final SQL, which keeps the
// builder correct for both "?" (SQLite) and "$n" (PostgreSQL) dialects.
type queryArgs struct {
	placeholder func(int) string
	args        []any
}

// newQueryArgs returns an empty queryArgs bound to the given dialect.
func (d *DB) newQueryArgs() *queryArgs {
	return &queryArgs{placeholder: d.dialect.Placeholder}
}

// bind appends v to the argument list and returns its placeholder.
func (q *queryArgs) bind(v any) string {
	q.args = append(q.args, v)
	return q.placeholder(len(q.args))
}
