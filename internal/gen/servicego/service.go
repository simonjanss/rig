package servicego

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// serviceFile emits the interface and the default implementation.
func (e *emitter) serviceFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.serviceInterface(b, res)
	e.childDeletesType(b, res)
	e.writerType(b, res)
	e.defaultService(b, res)

	return artifact(naming.Snake(res.Name)+"_service.gen.go", b, gen.Overwrite)
}

func (e *emitter) serviceInterface(b *gobuf.Buf, res *ir.Resource) {
	ctxPkg := b.Import("context")

	b.Comment(res.Name + "Service is the interface your service layer implements.\n\n" +
		"Embedding Default" + res.Name + "Service satisfies all of it, so a resource " +
		"with no business logic needs nothing but a constructor. Override a method " +
		"to add a rule and delegate to the embedded default for the rest.")
	b.L("type %sService interface {", res.Name)

	for i := range res.Endpoints {
		ep := &res.Endpoints[i]
		if i > 0 {
			b.NL()
		}
		if doc := endpointDoc(ep); doc != "" {
			b.Comment(doc)
		}
		b.L("%s", e.methodSignature(b, res, ep, ctxPkg))
	}

	if hasFiles(res) {
		b.NL()
		b.Comment("Files is where this resource's uploads go.\n\n" +
			"It is on the interface because the handler needs it too: the parts of " +
			"a multipart create have to be stored as they arrive, and a part's " +
			"body is only valid until the next one is asked for — so there is no " +
			"point at which the whole form could be handed over at once.")
		b.L("Files() *%s.Service", b.Import(filesModule))
	}

	if hasParents(res) {
		b.NL()
		b.Comment("ParentHooks is what this resource does when a row it points at " +
			"is deleted. [Link] reads it and hands it to the parent, which is the " +
			"only caller: the parent never sees this service, only the closures.")
		b.L("ParentHooks() %sParentHooks", res.Name)
	}
	if hasChildren(res) {
		b.NL()
		b.Comment("AdoptChildren receives the hooks of the tables referencing this " +
			"one. [Link] calls it.")
		b.L("AdoptChildren(%s)", childDeletesAlias(res))
	}

	b.L("}")
	b.NL()
}

// methodSignature renders one interface method.
func (e *emitter) methodSignature(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, ctxPkg string) string {
	request := "Request[" +
		e.slotType(b, res, ep, "path") + ", " +
		e.slotType(b, res, ep, "query") + ", " +
		e.slotType(b, res, ep, "body") + "]"

	extra := ""
	if ep.Name == ir.OpCreate && hasFiles(res) {
		// A create on a table with a file column also arrives as a form, and
		// what the form carried has to reach the same method: the row and its
		// files are committed together or the not-null column is unreachable.
		// It is nil on the JSON path, which is what leaves that path alone.
		extra = ", pending []*" + b.Import(filesModule) + ".Pending"
	}

	return ep.Impl.ServiceMethod + "(ctx " + ctxPkg + ".Context, r " + request + extra + ") " +
		e.returnType(b, res, ep)
}

// returnType is what an endpoint hands back.
func (e *emitter) returnType(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) string {
	if ep.File != nil {
		switch ep.Method {
		case "POST":
			return "(*" + e.fileShapeRef(b) + ", error)"
		case "GET":
			// The response, not a copy of it. Nothing reads ahead, which is what
			// lets a file larger than memory go straight to the wire — and it is
			// why the caller closes the body.
			return "(*" + b.Import(filesModule) + ".Content, error)"
		}
	}

	obj := successBodyObject(ep)
	if obj == "" {
		return "error"
	}
	return "(*" + e.objectType(b, res, obj) + ", error)"
}

// objectType resolves a response body's name to a Go type.
//
// The entity itself belongs to the model; everything else — a page of them, a
// shape declared in configuration — is this package's.
func (e *emitter) objectType(b *gobuf.Buf, res *ir.Resource, name string) string {
	if name == res.Name {
		return e.entity(b, res)
	}
	return e.objectRef(b, name)
}

// successBodyObject names the object a successful response carries, or empty
// for a response with no body.
func successBodyObject(ep *ir.Endpoint) string {
	for _, r := range ep.Responses {
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			return r.BodyObject
		}
	}
	return ""
}

func endpointDoc(ep *ir.Endpoint) string {
	doc := ep.Summary
	if ep.Description != "" {
		if doc != "" {
			doc += "\n\n"
		}
		doc += ep.Description
	}
	return doc
}

// writerType emits the writes with the service's own rules already attached.
//
// It exists for the endpoints rig cannot write. A custom endpoint that reaches
// for the repository directly has to remember to pass the hooks, and one that
// forgets is a second way into the table with a different set of rules — the
// exact failure the contract was introduced to prevent. Here the pairing is
// made once and cannot come apart.
//
// The generated operations go through it too, so there is one write path rather
// than one for rig and one for everybody else.
func (e *emitter) writerType(b *gobuf.Buf, res *ir.Resource) {
	if res.Storage == nil {
		return
	}
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		hookPkg = b.Import(runtimeModule + "/dbhook")
		store   = e.store(b)
		model   = e.model(b)
		entity  = e.entity(b, res)
		s       = res.Storage
	)
	name := res.Name + "Writer"

	b.Comment(name + " writes a " + res.Name + " with the service's rules attached.\n\n" +
		"Every write rig generates goes through it, and so should every write a " +
		"custom endpoint makes: reaching for the repository directly means " +
		"passing the hooks by hand, and forgetting once is a second way into the " +
		"table where the rules do not run.")
	b.L("type %s struct {", name)
	b.L("repo %s.%sRepository", store, res.Name)
	b.L("hooks %sHooks", res.Name)
	if hasChildren(res) {
		b.Comment("children is what the tables referencing this one want to happen " +
			"when a row goes. It is a pointer because it is filled in after the " +
			"writer exists: services are built one at a time and in no particular " +
			"order, and a child cannot register with a parent that is not " +
			"constructed yet.")
		b.L("children *%s", childDeletesAlias(res))
	}
	b.L("}")
	b.NL()

	b.Comment("New" + name + " pairs a repository with the rules that apply to it.")
	b.L("func New%s(repo %s.%sRepository, hooks %sHooks) %s {",
		name, store, res.Name, res.Name, name)
	if hasChildren(res) {
		b.L("return %s{repo: repo, hooks: hooks, children: new(%s)}", name, childDeletesAlias(res))
	} else {
		b.L("return %s{repo: repo, hooks: hooks}", name)
	}
	b.L("}")
	b.NL()

	e.adoptChildren(b, res, name)

	b.Comment("Create inserts a row, running the create rules and hooks.")
	b.L("func (w %s) Create(ctx %s.Context, in %s.%sCreateInput) (*%s, error) {",
		name, ctxPkg, model, res.Name, entity)
	b.L("return w.repo.Create(ctx, %s.Create[%s.%sCreateInput, %s]{Input: in, Hooks: w.hooks.Create})",
		hookPkg, model, res.Name, entity)
	b.L("}")
	b.NL()

	b.Comment("Update changes a row, running the update rules and hooks.")
	b.L("func (w %s) Update(ctx %s.Context, id %s.UUID, in %s.%sUpdateInput) (*%s, error) {",
		name, ctxPkg, uuidPkg, model, res.Name, entity)
	b.L("return w.repo.Update(ctx, id, %s.Update[%s.%sUpdateInput, %s]{Input: in, Hooks: w.hooks.Update})",
		hookPkg, model, res.Name, entity)
	b.L("}")
	b.NL()

	b.Comment("Delete removes a row, running the delete hooks.")
	b.L("func (w %s) Delete(ctx %s.Context, in %s.%sDeleteInput) error {",
		name, ctxPkg, model, res.Name)
	if hasChildren(res) {
		b.Comment("The children are read here rather than captured when the writer " +
			"was built, so a delete runs whatever is registered now. Before [Link] " +
			"has run there is nothing registered, and a delete is exactly what it " +
			"was before rig propagated anything.")
		b.L("hooks := w.hooks.Delete")
		b.L("hooks.Children = *w.children")
		b.L("return w.repo.Delete(ctx, %s.Delete[%s.%sDeleteInput, %s]{Input: in, Hooks: hooks})",
			hookPkg, model, res.Name, entity)
	} else {
		b.L("return w.repo.Delete(ctx, %s.Delete[%s.%sDeleteInput, %s]{Input: in, Hooks: w.hooks.Delete})",
			hookPkg, model, res.Name, entity)
	}
	b.L("}")
	b.NL()

	if s.IsSoftDeletable() {
		b.Comment("Restore brings a retired row back, running the restore hooks. " +
			"The input starts empty: what a row has to change to be allowed back " +
			"is the hook's decision, not the caller's.")
		b.L("func (w %s) Restore(ctx %s.Context, id %s.UUID) (*%s, error) {",
			name, ctxPkg, uuidPkg, entity)
		b.L("return w.repo.Restore(ctx, id, %s.Restore[%s.%sUpdateInput, %s]{Hooks: w.hooks.Restore})",
			hookPkg, model, res.Name, entity)
		b.L("}")
		b.NL()
	}

	if s.IsSnapshotable() {
		b.Comment("Revert replays a previous version, running the update rules and " +
			"hooks — because that is what it is.")
		b.L("func (w %s) Revert(ctx %s.Context, id, versionID %s.UUID) (*%s, error) {",
			name, ctxPkg, uuidPkg, entity)
		b.L("return w.repo.Revert(ctx, id, versionID, w.hooks.Update)")
		b.L("}")
		b.NL()
	}
}

// rulesInterface emits what the service layer hands over, and the front-door
// constructor that consumes it.
//
// The service layer describes itself and nothing more: the hooks it wants, the
// custom endpoints it implements, and a Bind that receives the writer built
// from those hooks. Nothing in it mentions the service type, which is what
// removes the two-step construction — a value that has to exist before it can
// describe itself, so that the thing described can be handed back to it.
func (e *emitter) rulesInterface(b *gobuf.Buf, res *ir.Resource) {
	store := e.store(b)
	name := res.Name + "Rules"

	b.Comment(name + " is everything the service layer supplies about " + res.Name +
		".\n\n" +
		"Implement it on a type of your own and hand it to New" + res.Name +
		"Service. It is an interface rather than a struct so that the rules and " +
		"the endpoints are one value: a resource whose configuration declares an " +
		"endpoint cannot be wired up without an implementation of it, and the " +
		"failure is at the call to the constructor rather than on the route.")
	b.L("type %s interface {", name)
	if len(customEndpoints(res)) > 0 {
		b.Comment("The endpoints the table configuration declares, which rig has " +
			"no way to write.")
		b.L("%sEndpoints", res.Name)
		b.NL()
	}
	b.Comment("Hooks is everything that happens around a write, plus what a read " +
		"answers with. It is called once, during construction.")
	b.L("Hooks() %sHooks", res.Name)
	b.NL()
	b.Comment("Bind receives the writer built from those hooks, so a custom " +
		"endpoint can write through the same path a generated operation does.\n\n" +
		"It is in the interface rather than looked for, so a service that never " +
		"writes says so with an empty body instead of finding out at runtime that " +
		"a misspelled method left it without one. rig calls it once, before " +
		"anything can reach a hook.")
	b.L("Bind(%sWriter)", res.Name)
	b.L("}")
	b.NL()

	b.Comment("New" + res.Name + "Service is the front door.\n\n" +
		"It asks the rules what they are, builds the writer from that, and hands " +
		"the writer back. The service layer never has to hold a half-built value " +
		"or name the type it is part of.")
	front := ""
	if hasFiles(res) {
		front = ", files *" + b.Import(filesModule) + ".Service"
	}
	b.L("func New%sService(repo %s.%sRepository, rules %s%s) Default%sService {",
		res.Name, store, res.Name, name, front, res.Name)
	if len(customEndpoints(res)) > 0 {
		b.L("svc := NewDefault%sService(repo, %sContract{Hooks: rules.Hooks(), Endpoints: rules}%s)",
			res.Name, res.Name, frontArg(res))
	} else {
		b.L("svc := NewDefault%sService(repo, %sContract{Hooks: rules.Hooks()}%s)",
			res.Name, res.Name, frontArg(res))
	}
	b.L("rules.Bind(svc.Writer())")
	b.L("return svc")
	b.L("}")
	b.NL()
}

// defaultService emits the working implementation.
func (e *emitter) defaultService(b *gobuf.Buf, res *ir.Resource) {
	if res.Storage == nil {
		return
	}
	var (
		ctxPkg = b.Import("context")
		store  = e.store(b)
	)

	b.Comment("Default" + res.Name + "Service implements every operation.\n\n" +
		"The generated ones it answers itself, by calling the repository. A " +
		"custom endpoint it hands to the contract, which is where the only " +
		"implementation there could be lives.")
	b.L("type Default%sService struct {", res.Name)
	b.L("repo %s.%sRepository", store, res.Name)
	b.L("contract %sContract", res.Name)
	b.Comment("write is the same writer a custom endpoint gets through " +
		"Writer, so the generated operations and the hand-written ones take one " +
		"path.")
	b.L("write %sWriter", res.Name)
	if hasFiles(res) {
		b.Comment("files is where this resource's uploads go. It is a parameter " +
			"of the constructor rather than something to set afterwards, because " +
			"a table with a file column has endpoints that cannot answer without " +
			"it.")
		b.L("files *%s.Service", b.Import(filesModule))
	}
	b.L("}")
	b.NL()

	e.contractStruct(b, res)
	e.endpointsInterface(b, res)
	e.hooksStruct(b, res)
	e.rulesInterface(b, res)

	b.Comment("NewDefault" + res.Name + "Service builds the default " +
		"implementation.\n\n" +
		"The contract is a parameter rather than a field left to default, " +
		"because a rule nobody attached is a rule that does not run, and nothing " +
		"at the call site would have said so. An empty " + res.Name +
		"Contract is still allowed — it is just a thing somebody wrote down.")
	filesParam, filesArg := "", ""
	if hasFiles(res) {
		filesParam = ", files *" + b.Import(filesModule) + ".Service"
		filesArg = ", files: files"
	}

	b.L("func NewDefault%sService(repo %s.%sRepository, contract %sContract%s) Default%sService {",
		res.Name, store, res.Name, res.Name, filesParam, res.Name)
	if len(customEndpoints(res)) > 0 {
		b.Comment("A nil set is not a service with no custom endpoints; it is " +
			"one whose custom endpoints all answer 500. Failing at startup beats " +
			"finding that out from a caller.")
		b.L("if contract.Endpoints == nil {")
		b.L("panic(\"api.NewDefault%sService: Contract.Endpoints is required: %s declares custom endpoints\")",
			res.Name, res.Storage.Table)
		b.L("}")
		b.NL()
	}
	if hasFiles(res) {
		b.Comment("A nil file service is a resource whose upload routes all " +
			"answer 500. Failing at startup beats finding that out from a caller.")
		b.L("if files == nil {")
		b.L("panic(\"api.NewDefault%sService: a file service is required: %s has a file column\")",
			res.Name, res.Storage.Table)
		b.L("}")
		b.NL()
	}
	b.L("return Default%sService{repo: repo, contract: contract, write: New%sWriter(repo, contract.Hooks)%s}",
		res.Name, res.Name, filesArg)
	b.L("}")
	b.NL()

	b.Comment("Writer is how a custom endpoint writes.\n\n" +
		"It is the repository with this service's own rules already attached, so " +
		"a hand-written endpoint takes the path a generated one takes. Reaching " +
		"for the repository instead means passing the hooks by hand, and " +
		"forgetting once is a second way into the table where the rules do not " +
		"run.\n\n" +
		"It is a method rather than something the service layer holds because " +
		"there is then nothing to wire, and nothing to wire wrongly.")
	b.L("func (s Default%sService) Writer() %sWriter { return s.write }", res.Name, res.Name)
	b.NL()

	if hasParents(res) {
		b.Comment("ParentHooks implements " + res.Name + "Service.")
		b.L("func (s Default%sService) ParentHooks() %sParentHooks { return s.contract.Hooks.Parents }",
			res.Name, res.Name)
		b.NL()
	}
	if hasChildren(res) {
		b.Comment("AdoptChildren implements " + res.Name + "Service.")
		b.L("func (s Default%sService) AdoptChildren(cs %s) { s.write.AdoptChildren(cs) }",
			res.Name, childDeletesAlias(res))
		b.NL()
	}

	e.readHelpers(b, res)

	if hasFiles(res) {
		e.fileServiceField(b, res)
		for _, fc := range res.Files {
			e.fileURLHelper(b, res, fc)
		}
	}

	for i := range res.Endpoints {
		ep := &res.Endpoints[i]
		e.defaultMethod(b, res, ep, ctxPkg, store)
	}
}

// customEndpoints are the ones the configuration declared, which rig has no
// way to implement.
func customEndpoints(res *ir.Resource) []*ir.Endpoint {
	var out []*ir.Endpoint
	for i := range res.Endpoints {
		if res.Endpoints[i].Impl.Kind == ir.EndpointCustom {
			out = append(out, &res.Endpoints[i])
		}
	}
	return out
}

// endpointsInterface emits the custom endpoints as one interface.
//
// An interface rather than a set of function fields, because the point is what
// the compiler does with it: declaring a new endpoint in the configuration adds
// a method here, and the service that no longer implements the set stops
// building. Function fields would leave the same mistake as a nil at runtime,
// on a route that had been answering fine the day before.
func (e *emitter) endpointsInterface(b *gobuf.Buf, res *ir.Resource) {
	custom := customEndpoints(res)
	if len(custom) == 0 {
		return
	}
	ctxPkg := b.Import("context")

	b.Comment(res.Name + "Endpoints are the endpoints the table configuration " +
		"declares.\n\n" +
		"There is nothing sensible for rig to do by default with any of them — " +
		"an endpoint nobody could describe from the schema is exactly the one " +
		"that has to be written — so the whole set is the service layer's, and " +
		"it arrives through the contract.")
	b.L("type %sEndpoints interface {", res.Name)
	for i, ep := range custom {
		if i > 0 {
			b.NL()
		}
		if doc := endpointDoc(ep); doc != "" {
			b.Comment(doc)
		}
		b.L("%s", e.methodSignature(b, res, ep, ctxPkg))
	}
	b.L("}")
	b.NL()
}

// contractStruct emits everything the service layer decides, in one value.
//
// It is one type so that the constructor can demand it: a service is not
// finished until somebody has said what its rules are, even if the answer is
// none.
func (e *emitter) contractStruct(b *gobuf.Buf, res *ir.Resource) {
	b.Comment(res.Name + "Contract is what the service layer owes " + res.Name +
		": everything about it the schema cannot describe.\n\n" +
		"The members are optional in the sense that an empty one runs nothing. " +
		"None of them is optional in the sense of being skippable: the " +
		"constructor takes this value, so a resource with no rules has an empty " +
		"literal somebody can read rather than an absence nobody can see.")
	b.L("type %sContract struct {", res.Name)
	b.Comment("Hooks is everything that happens around a write: the rules it " +
		"is checked against, and the callbacks that run with it. One set per " +
		"operation, because the rules for creating a row and for changing one " +
		"are different questions asked about different fields.")
	b.L("Hooks %sHooks", res.Name)
	if len(customEndpoints(res)) > 0 {
		b.NL()
		b.Comment("Endpoints holds the operations the configuration declared " +
			"and rig cannot write. It is required, because a resource that " +
			"declares one has no working answer without it.")
		b.L("Endpoints %sEndpoints", res.Name)
	}
	b.L("}")
	b.NL()
}

// hooksStruct emits the one place a service hangs its callbacks.
//
// One field per write rather than one flat list, so the signature of each hook
// says which operation it belongs to and what it is given: an update hook sees
// the row as it was, a delete hook sees only what is about to go.
func (e *emitter) hooksStruct(b *gobuf.Buf, res *ir.Resource) {
	var (
		hookPkg = b.Import(runtimeModule + "/dbhook")
		model   = e.model(b)
		s       = res.Storage
	)

	b.Comment(res.Name + "Hooks are the callbacks that run around each generated " +
		"write.\n\n" +
		"Before and After run inside the write's transaction, so returning an " +
		"error from either undoes the write. AfterCommit runs once it has " +
		"landed, which is where anything outside the database belongs.")
	b.L("type %sHooks struct {", res.Name)
	b.Comment("Read shapes what a read answers with. It is the one set that is " +
		"not about a write, and the only one that runs outside the repository — " +
		"a write reads the row it is about to change, and narrowing that read " +
		"would have the update judge a row that is not the stored one.")
	b.L("Read %s.ReadHooks[%s.%sFilter, %s.%s]",
		hookPkg, model, res.Name, model, res.Name)
	b.NL()
	b.L("Create %s.CreateHooks[%s.%sCreateInput, %s.%s]",
		hookPkg, model, res.Name, model, res.Name)
	b.L("Update %s.UpdateHooks[%s.%sUpdateInput, %s.%s]",
		hookPkg, model, res.Name, model, res.Name)
	b.L("Delete %s.DeleteHooks[%s.%sDeleteInput, %s.%s]",
		hookPkg, model, res.Name, model, res.Name)
	if s.IsSoftDeletable() {
		b.Comment("Restore takes the update input, because a restore may have to " +
			"change the row to be allowed back at all.")
		b.L("Restore %s.RestoreHooks[%s.%sUpdateInput, %s.%s]",
			hookPkg, model, res.Name, model, res.Name)
	}
	if hasParents(res) {
		b.NL()
		b.Comment("Parents is one field pair per foreign key this table has to " +
			"another resource: what to do when the row it points at is deleted. " +
			"They are here rather than under Delete because they are about " +
			"somebody else's delete, not this one's.")
		b.L("Parents %sParentHooks", res.Name)
	}
	b.L("}")
	b.NL()

	e.parentHooksStruct(b, res)
}

func (e *emitter) defaultMethod(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, ctxPkg, store string) {
	recv := "s Default" + res.Name + "Service"

	b.L("// %s implements %sService.", ep.Impl.ServiceMethod, res.Name)
	b.L("func (%s) %s {", recv, e.methodSignature(b, res, ep, ctxPkg))

	if ep.Impl.Kind == ir.EndpointCustom {
		e.delegateBody(b, res, ep)
		b.L("}")
		b.NL()
		return
	}

	if ep.File != nil {
		switch ep.Method {
		case "POST":
			e.uploadBody(b, res, ep)
		case "GET":
			e.downloadBody(b, res, ep)
		default:
			e.deleteFileBody(b, res, ep)
		}
		b.L("}")
		b.NL()
		return
	}

	switch ep.Name {
	case ir.OpCreate:
		if hasFiles(res) {
			e.createWithFilesBody(b, res)
			break
		}
		e.createBody(b, res, store)
	case ir.OpGet:
		e.getBody(b, res)
	case ir.OpList:
		e.listBody(b, res, store, false)
	case ir.OpSearch:
		e.listBody(b, res, store, true)
	case ir.OpListDeleted:
		e.listDeletedBody(b, res)
	case ir.OpRestore:
		e.restoreBody(b, res)
	case ir.OpUpdate:
		e.updateBody(b, res, store)
	case ir.OpDelete:
		e.deleteBody(b, res, store)
	case ir.OpVersions:
		e.versionsBody(b, res)
	case ir.OpRevert:
		e.revertBody(b, res)
	}

	b.L("}")
	b.NL()
}

// delegateBody hands a custom endpoint to the contract.
//
// The guard is for a service assembled without the constructor — the zero value
// of the default is reachable, and a nil interface there would panic somewhere
// less informative than this.
func (e *emitter) delegateBody(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	errPkg := b.Import(runtimeModule + "/rigerr")

	fail := "return "
	if successBodyObject(ep) != "" {
		fail = "return nil, "
	}

	b.L("if s.contract.Endpoints == nil {")
	b.L("%s%s.Internal(nil, \"%s.%s has no implementation\")", fail, errPkg, res.Name, ep.Name)
	b.L("}")
	b.L("return s.contract.Endpoints.%s(ctx, r)", ep.Impl.ServiceMethod)
}

// createBody hands the request body straight to the repository.
//
// They are the same type. The copy that used to be here was a field-by-field
// transcription between two structs with identical fields, and the only thing
// it could ever do differently from this was miss one.
func (e *emitter) createBody(b *gobuf.Buf, res *ir.Resource, store string) {
	b.L("return s.write.Create(ctx, r.Body)")
}

func (e *emitter) getBody(b *gobuf.Buf, res *ir.Resource) {
	b.L("row, err := s.repo.Get(ctx, r.Path.ID%s)", e.scopeArgs(b, res))
	b.L("if err != nil { return nil, err }")
	b.NL()
	b.Comment("Not narrowed: Get fetches by primary key, and there is no filter " +
		"to add a condition to. A rule about whether this caller may have this " +
		"row belongs in Rows, which is handed it.")
	b.L("if err := s.readRows(ctx, r.Claims, []*%s{row}); err != nil { return nil, err }", e.entity(b, res))
	b.L("return row, nil")
}

// scopeArgs renders the read options a read passes on, as a trailing argument
// list.
//
// Nothing at all for a table that is not owner-scoped, which is why it returns
// text to append rather than a value: a table with no notion of "own" should not
// grow an empty options slice on every call for the sake of uniformity.
//
// The service trusts r.Query.Scope. It can: the handler parsed it, refused an
// unrecognised value, and refused a widening the caller does not hold a
// permission for. A caller reaching this method from Go rather than from HTTP is
// application code, which can already call the repository with any option it
// likes.
func (e *emitter) scopeArgs(b *gobuf.Buf, res *ir.Resource) string {
	if !res.Storage.IsOwnerScoped() {
		return ""
	}
	return ", readScope(r.Query.Scope)..."
}

// anyOwnerScoped reports whether the package needs the scope helper at all. A
// project where no table asked to be read narrowly gets neither the function nor
// the import.
func (e *emitter) anyOwnerScoped() bool {
	for i := range e.doc.API.Resources {
		if e.doc.API.Resources[i].Storage.IsOwnerScoped() {
			return true
		}
	}
	return false
}

// scopeHelper emits the one function that turns a requested scope into a read
// option.
//
// One function per package rather than three lines inlined into five reads: the
// mapping from "all" to dropping the filter is the security-relevant half of this
// feature, and it should be in one place where it can be read and tested.
func (e *emitter) scopeHelper(b *gobuf.Buf) {
	optPkg := b.Import(runtimeModule + "/readopt")
	tenPkg := b.Import(runtimeModule + "/tenancy")

	b.Comment("readScope turns a requested scope into the read options that " +
		"produce it.\n\n" +
		"Only \"all\" does anything. Every other value — including a zero value " +
		"from a caller that never set the field — leaves the narrow default in " +
		"place, so the failure mode of forgetting this is too few rows rather than " +
		"too many.")
	b.L("func readScope(s %s.Scope) []%s.Option {", tenPkg, optPkg)
	b.L("if s == %s.ScopeAll {", tenPkg)
	b.L("return []%s.Option{%s.WithoutOwnerScope()}", optPkg, optPkg)
	b.L("}")
	b.L("return nil")
	b.L("}")
	b.NL()
}

// readHelpers emit the two calls every generated read shares.
//
// Methods rather than inline blocks: five reads would otherwise carry the same
// six lines, and a hook that fires on four of them is worse than one that fires
// on none.
func (e *emitter) readHelpers(b *gobuf.Buf, res *ir.Resource) {
	if res.Storage == nil {
		return
	}
	var (
		ctxPkg = b.Import("context")
		tenPkg = b.Import(runtimeModule + "/tenancy")
		model  = e.model(b)
		entity = e.entity(b, res)
	)

	b.Comment("readFilter combines the caller's filter with whatever the read " +
		"hook narrows to.\n\n" +
		"Nested rather than merged, because the two are combined with AND: a " +
		"caller whose own filter is an OR cannot widen its way out of the one " +
		"the service added.")
	b.L("func (s Default%sService) readFilter(ctx %s.Context, claims %s.Claims, asked %s.%sFilter) (%s.%sFilter, error) {",
		res.Name, ctxPkg, tenPkg, model, res.Name, model, res.Name)
	b.L("if s.contract.Hooks.Read.Narrow == nil { return asked, nil }")
	b.NL()
	b.L("narrowed, err := s.contract.Hooks.Read.Narrow(ctx, caller(claims))")
	b.L("if err != nil { return asked, err }")
	b.L("if narrowed == nil { return asked, nil }")
	b.NL()
	b.L("return %s.%sFilter{NestedFilters: []%s.%sFilter{*narrowed, asked}}, nil",
		model, res.Name, model, res.Name)
	b.L("}")
	b.NL()

	b.Comment("readRows runs the read hook over what a read is about to answer " +
		"with.")
	b.L("func (s Default%sService) readRows(ctx %s.Context, claims %s.Claims, rows []*%s) error {",
		res.Name, ctxPkg, tenPkg, entity)
	b.L("if s.contract.Hooks.Read.Rows == nil { return nil }")
	b.L("return s.contract.Hooks.Read.Rows(ctx, caller(claims), rows)")
	b.L("}")
	b.NL()
}

// listBody emits List and Search, which differ only in whether a filter is
// applied.
func (e *emitter) listBody(b *gobuf.Buf, res *ir.Resource, store string, withFilter bool) {
	model := e.model(b)

	b.L("page := %s.%sPage{Limit: r.Query.Limit, Offset: r.Query.Offset}", model, res.Name)

	if withFilter {
		// The body's filter is the repository's filter — the same type, so
		// there is nothing here to copy and nothing to leave out.
		b.L("asked := r.Body.Filter")
	} else {
		b.L("asked := %s.New%sFilter()", model, res.Name)
	}
	b.L("filter, err := s.readFilter(ctx, r.Claims, asked)")
	b.L("if err != nil { return nil, err }")
	b.NL()

	b.L("rows, total, err := s.repo.List(ctx, filter, page%s)", e.scopeArgs(b, res))
	b.L("if err != nil { return nil, err }")
	b.L("if err := s.readRows(ctx, r.Claims, rows); err != nil { return nil, err }")
	b.NL()
	b.L("return &%sListResponse{", res.Name)
	b.L("Data: rows,")
	b.L("Pagination: Pagination{Offset: page.Offset, Limit: page.Limit, Total: total},")
	b.L("}, nil")
}

func (e *emitter) updateBody(b *gobuf.Buf, res *ir.Resource, store string) {
	b.L("return s.write.Update(ctx, r.Path.ID, r.Body)")
}

// listDeletedBody reads the trash.
//
// A separate repository call rather than a read option on List, because what it
// returns is not a subset of what List returns: the lifecycle predicate is
// inverted, and a caller that could ask List for deleted rows could also ask it
// for both at once, which is a page nobody can render.
func (e *emitter) listDeletedBody(b *gobuf.Buf, res *ir.Resource) {
	model := e.model(b)

	b.L("page := %s.%sPage{Limit: r.Query.Limit, Offset: r.Query.Offset}", model, res.Name)
	b.L("filter, err := s.readFilter(ctx, r.Claims, %s.New%sFilter())", model, res.Name)
	b.L("if err != nil { return nil, err }")
	b.NL()
	b.L("rows, total, err := s.repo.ListDeleted(ctx, filter, page%s)", e.scopeArgs(b, res))
	b.L("if err != nil { return nil, err }")
	b.L("if err := s.readRows(ctx, r.Claims, rows); err != nil { return nil, err }")
	b.NL()
	b.L("return &%sListResponse{", res.Name)
	b.L("Data: rows,")
	b.L("Pagination: Pagination{Offset: page.Offset, Limit: page.Limit, Total: total},")
	b.L("}, nil")
}

// restoreBody clears the deletion stamp.
//
// The input starts empty and stays that way unless the restore hook fills it
// in: the request carries an identifier, and what to do about a world that has
// moved on is the service layer's decision rather than the caller's.
func (e *emitter) restoreBody(b *gobuf.Buf, res *ir.Resource) {
	b.L("return s.write.Restore(ctx, r.Path.ID)")
}

// versionsBody reads a row's history.
//
// The Get first is not redundant. ListSnapshots on a row that is not there — or
// belongs to another tenant — returns an empty list, and answering 200 with
// nothing would confirm the identifier is real to somebody who cannot read it.
func (e *emitter) versionsBody(b *gobuf.Buf, res *ir.Resource) {
	b.L("if _, err := s.repo.Get(ctx, r.Path.ID%s); err != nil { return nil, err }", e.scopeArgs(b, res))
	b.NL()
	b.L("versions, err := s.repo.ListSnapshots(ctx, r.Path.ID)")
	b.L("if err != nil { return nil, err }")
	b.L("if err := s.readRows(ctx, r.Claims, versions); err != nil { return nil, err }")
	b.NL()
	b.Comment("A history is not paged: the whole of it comes back, and the " +
		"pagination block says exactly that rather than describing a page nobody " +
		"asked for.")
	b.L("return &%sListResponse{", res.Name)
	b.L("Data: versions,")
	b.L("Pagination: Pagination{Limit: len(versions), Total: int64(len(versions))},")
	b.L("}, nil")
}

// revertBody hands the replay to the repository.
//
// The update hooks, because a revert is an update: the same validator sees the
// values going in, and a rule that would refuse them now still refuses them.
func (e *emitter) revertBody(b *gobuf.Buf, res *ir.Resource) {
	b.L("return s.write.Revert(ctx, r.Path.ID, r.Body.VersionID)")
}

func (e *emitter) deleteBody(b *gobuf.Buf, res *ir.Resource, store string) {
	b.L("return s.write.Delete(ctx, %s.%sDeleteInput{ID: r.Path.ID})", e.model(b), res.Name)
}

// frontArg passes the file service through the front door when there is one.
func frontArg(res *ir.Resource) string {
	if hasFiles(res) {
		return ", files"
	}
	return ""
}
