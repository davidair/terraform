package terraform

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/agext/levenshtein"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/configs/configschema"
	"github.com/hashicorp/terraform/instances"
	"github.com/hashicorp/terraform/lang"
	"github.com/hashicorp/terraform/plans"
	"github.com/hashicorp/terraform/states"
	"github.com/hashicorp/terraform/tfdiags"
)

// Evaluator provides the necessary contextual data for evaluating expressions
// for a particular walk operation.
type Evaluator struct {
	// Operation defines what type of operation this evaluator is being used
	// for.
	Operation walkOperation

	// Meta is contextual metadata about the current operation.
	Meta *ContextMeta

	// Config is the root node in the configuration tree.
	Config *configs.Config

	// VariableValues is a map from variable names to their associated values,
	// within the module indicated by ModulePath. VariableValues is modified
	// concurrently, and so it must be accessed only while holding
	// VariableValuesLock.
	//
	// The first map level is string representations of addr.ModuleInstance
	// values, while the second level is variable names.
	VariableValues     map[string]map[string]cty.Value
	VariableValuesLock *sync.Mutex

	// Schemas is a repository of all of the schemas we should need to
	// evaluate expressions. This must be constructed by the caller to
	// include schemas for all of the providers, resource types, data sources
	// and provisioners used by the given configuration and state.
	//
	// This must not be mutated during evaluation.
	Schemas *Schemas

	// State is the current state, embedded in a wrapper that ensures that
	// it can be safely accessed and modified concurrently.
	State *states.SyncState

	// Changes is the set of proposed changes, embedded in a wrapper that
	// ensures they can be safely accessed and modified concurrently.
	Changes *plans.ChangesSync
}

// Scope creates an evaluation scope for the given module path and optional
// resource.
//
// If the "self" argument is nil then the "self" object is not available
// in evaluated expressions. Otherwise, it behaves as an alias for the given
// address.
func (e *Evaluator) Scope(data lang.Data, self addrs.Referenceable) *lang.Scope {
	return &lang.Scope{
		Data:     data,
		SelfAddr: self,
		PureOnly: e.Operation != walkApply && e.Operation != walkDestroy && e.Operation != walkEval,
		BaseDir:  ".", // Always current working directory for now.
	}
}

// evaluationStateData is an implementation of lang.Data that resolves
// references primarily (but not exclusively) using information from a State.
type evaluationStateData struct {
	Evaluator *Evaluator

	// ModulePath is the path through the dynamic module tree to the module
	// that references will be resolved relative to.
	ModulePath addrs.ModuleInstance

	// InstanceKeyData describes the values, if any, that are accessible due
	// to repetition of a containing object using "count" or "for_each"
	// arguments. (It is _not_ used for the for_each inside "dynamic" blocks,
	// since the user specifies in that case which variable name to locally
	// shadow.)
	InstanceKeyData InstanceKeyEvalData

	// Operation records the type of walk the evaluationStateData is being used
	// for.
	Operation walkOperation
}

// InstanceKeyEvalData is the old name for instances.RepetitionData, aliased
// here for compatibility. In new code, use instances.RepetitionData instead.
type InstanceKeyEvalData = instances.RepetitionData

// EvalDataForInstanceKey constructs a suitable InstanceKeyEvalData for
// evaluating in a context that has the given instance key.
//
// The forEachMap argument can be nil when preparing for evaluation
// in a context where each.value is prohibited, such as a destroy-time
// provisioner. In that case, the returned EachValue will always be
// cty.NilVal.
func EvalDataForInstanceKey(key addrs.InstanceKey, forEachMap map[string]cty.Value) InstanceKeyEvalData {
	var evalData InstanceKeyEvalData
	if key == nil {
		return evalData
	}

	keyValue := key.Value()
	switch keyValue.Type() {
	case cty.String:
		evalData.EachKey = keyValue
		evalData.EachValue = forEachMap[keyValue.AsString()]
	case cty.Number:
		evalData.CountIndex = keyValue
	}
	return evalData
}

// EvalDataForNoInstanceKey is a value of InstanceKeyData that sets no instance
// key values at all, suitable for use in contexts where no keyed instance
// is relevant.
var EvalDataForNoInstanceKey = InstanceKeyEvalData{}

// evaluationStateData must implement lang.Data
var _ lang.Data = (*evaluationStateData)(nil)

func (d *evaluationStateData) GetCountAttr(addr addrs.CountAttr, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	switch addr.Name {

	case "index":
		idxVal := d.InstanceKeyData.CountIndex
		if idxVal == cty.NilVal {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `Reference to "count" in non-counted context`,
				Detail:   fmt.Sprintf(`The "count" object can only be used in "module", "resource", and "data" blocks, and only when the "count" argument is set.`),
				Subject:  rng.ToHCL().Ptr(),
			})
			return cty.UnknownVal(cty.Number), diags
		}
		return idxVal, diags

	default:
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Invalid "count" attribute`,
			Detail:   fmt.Sprintf(`The "count" object does not have an attribute named %q. The only supported attribute is count.index, which is the index of each instance of a resource block that has the "count" argument set.`, addr.Name),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags
	}
}

func (d *evaluationStateData) GetForEachAttr(addr addrs.ForEachAttr, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	var returnVal cty.Value
	switch addr.Name {

	case "key":
		returnVal = d.InstanceKeyData.EachKey
	case "value":
		returnVal = d.InstanceKeyData.EachValue

		if returnVal == cty.NilVal {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `each.value cannot be used in this context`,
				Detail:   fmt.Sprintf(`A reference to "each.value" has been used in a context in which it unavailable, such as when the configuration no longer contains the value in its "for_each" expression. Remove this reference to each.value in your configuration to work around this error.`),
				Subject:  rng.ToHCL().Ptr(),
			})
			return cty.UnknownVal(cty.DynamicPseudoType), diags
		}
	default:
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Invalid "each" attribute`,
			Detail:   fmt.Sprintf(`The "each" object does not have an attribute named %q. The supported attributes are each.key and each.value, the current key and value pair of the "for_each" attribute set.`, addr.Name),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags
	}

	if returnVal == cty.NilVal {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Reference to "each" in context without for_each`,
			Detail:   fmt.Sprintf(`The "each" object can be used only in "module" or "resource" blocks, and only when the "for_each" argument is set.`),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.UnknownVal(cty.DynamicPseudoType), diags
	}
	return returnVal, diags
}

func (d *evaluationStateData) GetInputVariable(addr addrs.InputVariable, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// First we'll make sure the requested value is declared in configuration,
	// so we can produce a nice message if not.
	moduleConfig := d.Evaluator.Config.DescendentForInstance(d.ModulePath)
	if moduleConfig == nil {
		// should never happen, since we can't be evaluating in a module
		// that wasn't mentioned in configuration.
		panic(fmt.Sprintf("input variable read from %s, which has no configuration", d.ModulePath))
	}

	config := moduleConfig.Module.Variables[addr.Name]
	if config == nil {
		var suggestions []string
		for k := range moduleConfig.Module.Variables {
			suggestions = append(suggestions, k)
		}
		suggestion := nameSuggestion(addr.Name, suggestions)
		if suggestion != "" {
			suggestion = fmt.Sprintf(" Did you mean %q?", suggestion)
		} else {
			suggestion = fmt.Sprintf(" This variable can be declared with a variable %q {} block.", addr.Name)
		}

		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Reference to undeclared input variable`,
			Detail:   fmt.Sprintf(`An input variable with the name %q has not been declared.%s`, addr.Name, suggestion),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags
	}

	wantType := cty.DynamicPseudoType
	if config.Type != cty.NilType {
		wantType = config.Type
	}

	d.Evaluator.VariableValuesLock.Lock()
	defer d.Evaluator.VariableValuesLock.Unlock()

	// During the validate walk, input variables are always unknown so
	// that we are validating the configuration for all possible input values
	// rather than for a specific set. Checking against a specific set of
	// input values then happens during the plan walk.
	//
	// This is important because otherwise the validation walk will tend to be
	// overly strict, requiring expressions throughout the configuration to
	// be complicated to accommodate all possible inputs, whereas returning
	// known here allows for simpler patterns like using input values as
	// guards to broadly enable/disable resources, avoid processing things
	// that are disabled, etc. Terraform's static validation leans towards
	// being liberal in what it accepts because the subsequent plan walk has
	// more information available and so can be more conservative.
	if d.Operation == walkValidate {
		return cty.UnknownVal(wantType), diags
	}

	moduleAddrStr := d.ModulePath.String()
	vals := d.Evaluator.VariableValues[moduleAddrStr]
	if vals == nil {
		return cty.UnknownVal(wantType), diags
	}

	val, isSet := vals[addr.Name]
	if !isSet {
		if config.Default != cty.NilVal {
			return config.Default, diags
		}
		return cty.UnknownVal(wantType), diags
	}

	var err error
	val, err = convert.Convert(val, wantType)
	if err != nil {
		// We should never get here because this problem should've been caught
		// during earlier validation, but we'll do something reasonable anyway.
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Incorrect variable type`,
			Detail:   fmt.Sprintf(`The resolved value of variable %q is not appropriate: %s.`, addr.Name, err),
			Subject:  &config.DeclRange,
		})
		// Stub out our return value so that the semantic checker doesn't
		// produce redundant downstream errors.
		val = cty.UnknownVal(wantType)
	}

	return val, diags
}

func (d *evaluationStateData) GetLocalValue(addr addrs.LocalValue, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// First we'll make sure the requested value is declared in configuration,
	// so we can produce a nice message if not.
	moduleConfig := d.Evaluator.Config.DescendentForInstance(d.ModulePath)
	if moduleConfig == nil {
		// should never happen, since we can't be evaluating in a module
		// that wasn't mentioned in configuration.
		panic(fmt.Sprintf("local value read from %s, which has no configuration", d.ModulePath))
	}

	config := moduleConfig.Module.Locals[addr.Name]
	if config == nil {
		var suggestions []string
		for k := range moduleConfig.Module.Locals {
			suggestions = append(suggestions, k)
		}
		suggestion := nameSuggestion(addr.Name, suggestions)
		if suggestion != "" {
			suggestion = fmt.Sprintf(" Did you mean %q?", suggestion)
		}

		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Reference to undeclared local value`,
			Detail:   fmt.Sprintf(`A local value with the name %q has not been declared.%s`, addr.Name, suggestion),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags
	}

	val := d.Evaluator.State.LocalValue(addr.Absolute(d.ModulePath))
	if val == cty.NilVal {
		// Not evaluated yet?
		val = cty.DynamicVal
	}

	return val, diags
}

func (d *evaluationStateData) GetModule(addr addrs.ModuleCall, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// Output results live in the module that declares them, which is one of
	// the child module instances of our current module path.
	moduleAddr := d.ModulePath.Module().Child(addr.Name)

	parentCfg := d.Evaluator.Config.DescendentForInstance(d.ModulePath)
	callConfig, ok := parentCfg.Module.ModuleCalls[addr.Name]
	if !ok {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Reference to undeclared module`,
			Detail:   fmt.Sprintf(`The configuration contains no %s.`, moduleAddr),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags
	}

	// We'll consult the configuration to see what output names we are
	// expecting, so we can ensure the resulting object is of the expected
	// type even if our data is incomplete for some reason.
	moduleConfig := d.Evaluator.Config.Descendent(moduleAddr)
	if moduleConfig == nil {
		// should never happen, since we have a valid module call above, this
		// should be caught during static validation.
		panic(fmt.Sprintf("output value read from %s, which has no configuration", moduleAddr))
	}
	outputConfigs := moduleConfig.Module.Outputs

	// Collect all the relevant outputs that current exist in the state.
	// We know the instance path up to this point, and the child module name,
	// so we only need to store these by instance key.
	stateMap := map[addrs.InstanceKey]map[string]cty.Value{}
	for _, output := range d.Evaluator.State.ModuleOutputs(d.ModulePath, addr) {
		_, callInstance := output.Addr.Module.CallInstance()
		instance, ok := stateMap[callInstance.Key]
		if !ok {
			instance = map[string]cty.Value{}
			stateMap[callInstance.Key] = instance
		}

		instance[output.Addr.OutputValue.Name] = output.Value
	}

	// Get all changes that reside for this module call within our path.
	// The change contains the full addr, so we can key these with strings.
	changesMap := map[addrs.InstanceKey]map[string]*plans.OutputChangeSrc{}
	for _, change := range d.Evaluator.Changes.GetOutputChanges(d.ModulePath, addr) {
		_, callInstance := change.Addr.Module.CallInstance()
		instance, ok := changesMap[callInstance.Key]
		if !ok {
			instance = map[string]*plans.OutputChangeSrc{}
			changesMap[callInstance.Key] = instance
		}

		instance[change.Addr.OutputValue.Name] = change
	}

	// Build up all the module objects, creating a map of values for each
	// module instance.
	moduleInstances := map[addrs.InstanceKey]map[string]cty.Value{}

	// create a dummy object type for validation below
	unknownMap := map[string]cty.Type{}

	// the structure is based on the configuration, so iterate through all the
	// defined outputs, and add any instance state or changes we find.
	for _, cfg := range outputConfigs {
		// record the output names for validation
		unknownMap[cfg.Name] = cty.DynamicPseudoType

		// get all instance output for this path from the state
		for key, states := range stateMap {
			outputState, ok := states[cfg.Name]
			if !ok {
				continue
			}

			instance, ok := moduleInstances[key]
			if !ok {
				instance = map[string]cty.Value{}
				moduleInstances[key] = instance
			}

			instance[cfg.Name] = outputState
		}

		// any pending changes override the state state values
		for key, changes := range changesMap {
			changeSrc, ok := changes[cfg.Name]
			if !ok {
				continue
			}

			instance, ok := moduleInstances[key]
			if !ok {
				instance = map[string]cty.Value{}
				moduleInstances[key] = instance
			}

			change, err := changeSrc.Decode()
			if err != nil {
				// This should happen only if someone has tampered with a plan
				// file, so we won't bother with a pretty error for it.
				diags = diags.Append(fmt.Errorf("planned change for %s could not be decoded: %s", addr, err))
				instance[cfg.Name] = cty.DynamicVal
				continue
			}

			instance[cfg.Name] = change.After
		}
	}

	var ret cty.Value

	// compile the outputs into the correct value type for the each mode
	switch {
	case callConfig.Count != nil:
		// figure out what the last index we have is
		length := -1
		for key := range moduleInstances {
			intKey, ok := key.(addrs.IntKey)
			if !ok {
				// old key from state which is being dropped
				continue
			}
			if int(intKey) >= length {
				length = int(intKey) + 1
			}
		}

		if length > 0 {
			vals := make([]cty.Value, length)
			for key, instance := range moduleInstances {
				intKey, ok := key.(addrs.IntKey)
				if !ok {
					// old key from state which is being dropped
					continue
				}

				vals[int(intKey)] = cty.ObjectVal(instance)
			}

			// Insert unknown values where there are any missing instances
			for i, v := range vals {
				if v.IsNull() {
					vals[i] = cty.DynamicVal
					continue
				}
			}
			ret = cty.TupleVal(vals)
		} else {
			ret = cty.EmptyTupleVal
		}

	case callConfig.ForEach != nil:
		vals := make(map[string]cty.Value)
		for key, instance := range moduleInstances {
			strKey, ok := key.(addrs.StringKey)
			if !ok {
				continue
			}

			vals[string(strKey)] = cty.ObjectVal(instance)
		}

		if len(vals) > 0 {
			ret = cty.ObjectVal(vals)
		} else {
			ret = cty.EmptyObjectVal
		}

	default:
		val, ok := moduleInstances[addrs.NoKey]
		if !ok {
			// create the object if there wasn't one known
			val = map[string]cty.Value{}
			for k := range outputConfigs {
				val[k] = cty.DynamicVal
			}
		}

		ret = cty.ObjectVal(val)
	}

	// The module won't be expanded during validation, so we need to return an
	// unknown value. This will ensure the types looks correct, since we built
	// the objects based on the configuration.
	if d.Operation == walkValidate {
		// While we know the type here and it would be nice to validate whether
		// indexes are valid or not, because tuples and objects have fixed
		// numbers of elements we can't simply return an unknown value of the
		// same type since we have not expanded any instances during
		// validation.
		//
		// In order to validate the expression a little precisely, we'll create
		// an unknown map or list here to get more type information.
		ty := cty.Object(unknownMap)
		switch {
		case callConfig.Count != nil:
			ret = cty.UnknownVal(cty.List(ty))
		case callConfig.ForEach != nil:
			ret = cty.UnknownVal(cty.Map(ty))
		default:
			ret = cty.UnknownVal(ty)
		}
	}

	return ret, diags
}

func (d *evaluationStateData) GetPathAttr(addr addrs.PathAttr, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	switch addr.Name {

	case "cwd":
		wd, err := os.Getwd()
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `Failed to get working directory`,
				Detail:   fmt.Sprintf(`The value for path.cwd cannot be determined due to a system error: %s`, err),
				Subject:  rng.ToHCL().Ptr(),
			})
			return cty.DynamicVal, diags
		}
		return cty.StringVal(filepath.ToSlash(wd)), diags

	case "module":
		moduleConfig := d.Evaluator.Config.DescendentForInstance(d.ModulePath)
		if moduleConfig == nil {
			// should never happen, since we can't be evaluating in a module
			// that wasn't mentioned in configuration.
			panic(fmt.Sprintf("module.path read from module %s, which has no configuration", d.ModulePath))
		}
		sourceDir := moduleConfig.Module.SourceDir
		return cty.StringVal(filepath.ToSlash(sourceDir)), diags

	case "root":
		sourceDir := d.Evaluator.Config.Module.SourceDir
		return cty.StringVal(filepath.ToSlash(sourceDir)), diags

	default:
		suggestion := nameSuggestion(addr.Name, []string{"cwd", "module", "root"})
		if suggestion != "" {
			suggestion = fmt.Sprintf(" Did you mean %q?", suggestion)
		}
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Invalid "path" attribute`,
			Detail:   fmt.Sprintf(`The "path" object does not have an attribute named %q.%s`, addr.Name, suggestion),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags
	}
}

func (d *evaluationStateData) GetResource(addr addrs.Resource, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	// First we'll consult the configuration to see if an resource of this
	// name is declared at all.
	moduleAddr := d.ModulePath
	moduleConfig := d.Evaluator.Config.DescendentForInstance(moduleAddr)
	if moduleConfig == nil {
		// should never happen, since we can't be evaluating in a module
		// that wasn't mentioned in configuration.
		panic(fmt.Sprintf("resource value read from %s, which has no configuration", moduleAddr))
	}

	config := moduleConfig.Module.ResourceByAddr(addr)
	if config == nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Reference to undeclared resource`,
			Detail:   fmt.Sprintf(`A resource %q %q has not been declared in %s`, addr.Type, addr.Name, moduleDisplayAddr(moduleAddr)),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags
	}

	rs := d.Evaluator.State.Resource(addr.Absolute(d.ModulePath))

	if rs == nil {
		// we must return DynamicVal so that both interpretations
		// can proceed without generating errors, and we'll deal with this
		// in a later step where more information is gathered.
		// (In practice we should only end up here during the validate walk,
		// since later walks should have at least partial states populated
		// for all resources in the configuration.)
		return cty.DynamicVal, diags
	}

	providerAddr := rs.ProviderConfig

	schema := d.getResourceSchema(addr, providerAddr)
	if schema == nil {
		// This shouldn't happen, since validation before we get here should've
		// taken care of it, but we'll show a reasonable error message anyway.
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Missing resource type schema`,
			Detail:   fmt.Sprintf("No schema is available for %s in %s. This is a bug in Terraform and should be reported.", addr, providerAddr),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags
	}
	ty := schema.ImpliedType()

	// See if this entire resource is being destroyed which can change how we
	// evaluate it.
	//
	// We have a problem where during a full destroy all instances
	// will have a Delete change planned, but we may need to evaluate some
	// resources for providers that reference them to work correctly.
	//
	// If all instances are being removed, there are only 2 possible cases,
	// either this is a full destroy and a provider is referencing this,
	// directly or through temporary values; or this is an invalid reference
	// and the error should have been caught elsewhere.
	//
	// If we are destroying the entire resource, then we allow the evaluation
	// of the instances being destroyed, since the end usage should be limited
	// as noted above. We record the changes here only so we don't need to look
	// them up again in the next loop.
	fullDestroy := true
	changes := map[addrs.InstanceKey]*plans.ResourceInstanceChangeSrc{}
	for key := range rs.Instances {
		instAddr := addr.Instance(key).Absolute(d.ModulePath)
		change := d.Evaluator.Changes.GetResourceInstanceChange(instAddr, states.CurrentGen)
		changes[key] = change
		if change != nil {
			if change.Action != plans.Delete && fullDestroy {
				fullDestroy = false
			}
		}

	}

	// Decode all instances in the current state
	instances := map[addrs.InstanceKey]cty.Value{}
	for key, is := range rs.Instances {
		if is == nil || is.Current == nil {
			// Assume we're dealing with an instance that hasn't been created yet.
			instances[key] = cty.UnknownVal(ty)
			continue
		}
		instAddr := addr.Instance(key).Absolute(d.ModulePath)

		change := changes[key]
		if change != nil {
			// Don't take any resources that are yet to be deleted into account.
			// If the referenced resource is CreateBeforeDestroy, then orphaned
			// instances will be in the state, as they are not destroyed until
			// after their dependants are updated.
			if change.Action == plans.Delete {
				if !fullDestroy {
					continue
				}
				log.Printf("[WARN] evaluating %s, which is planned to be deleted", instAddr)
			}
		}

		// Planned resources are temporarily stored in state with empty values,
		// and need to be replaced by the planned value here.
		if is.Current.Status == states.ObjectPlanned {
			if change == nil {
				// If the object is in planned status then we should not get
				// here, since we should have found a pending value in the plan
				// above instead.
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Missing pending object in plan",
					Detail:   fmt.Sprintf("Instance %s is marked as having a change pending but that change is not recorded in the plan. This is a bug in Terraform; please report it.", instAddr),
					Subject:  &config.DeclRange,
				})
				continue
			}
			val, err := change.After.Decode(ty)
			if err != nil {
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid resource instance data in plan",
					Detail:   fmt.Sprintf("Instance %s data could not be decoded from the plan: %s.", instAddr, err),
					Subject:  &config.DeclRange,
				})
				continue
			}

			instances[key] = val
			continue
		}

		ios, err := is.Current.Decode(ty)
		if err != nil {
			// This shouldn't happen, since by the time we get here we
			// should have upgraded the state data already.
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid resource instance data in state",
				Detail:   fmt.Sprintf("Instance %s data could not be decoded from the state: %s.", instAddr, err),
				Subject:  &config.DeclRange,
			})
			continue
		}
		instances[key] = ios.Value
	}

	var ret cty.Value

	switch {
	case config.Count != nil:
		// figure out what the last index we have is
		length := -1
		for key := range instances {
			intKey, ok := key.(addrs.IntKey)
			if !ok {
				continue
			}
			if int(intKey) >= length {
				length = int(intKey) + 1
			}
		}

		if length > 0 {
			vals := make([]cty.Value, length)
			for key, instance := range instances {
				intKey, ok := key.(addrs.IntKey)
				if !ok {
					// old key from state, which isn't valid for evaluation
					continue
				}

				vals[int(intKey)] = instance
			}

			// Insert unknown values where there are any missing instances
			for i, v := range vals {
				if v == cty.NilVal {
					vals[i] = cty.UnknownVal(ty)
				}
			}
			ret = cty.TupleVal(vals)
		} else {
			ret = cty.EmptyTupleVal
		}

	case config.ForEach != nil:
		vals := make(map[string]cty.Value)
		for key, instance := range instances {
			strKey, ok := key.(addrs.StringKey)
			if !ok {
				// old key that is being dropped and not used for evaluation
				continue
			}
			vals[string(strKey)] = instance
		}

		if len(vals) > 0 {
			// We use an object rather than a map here because resource schemas
			// may include dynamically-typed attributes, which will then cause
			// each instance to potentially have a different runtime type even
			// though they all conform to the static schema.
			ret = cty.ObjectVal(vals)
		} else {
			ret = cty.EmptyObjectVal
		}

	default:
		val, ok := instances[addrs.NoKey]
		if !ok {
			// if the instance is missing, insert an unknown value
			val = cty.UnknownVal(ty)
		}

		ret = val
	}

	// since the plan was not yet created during validate, the values we
	// collected here may not correspond with configuration, so they must be
	// unknown.
	if d.Operation == walkValidate {
		// While we know the type here and it would be nice to validate whether
		// indexes are valid or not, because tuples and objects have fixed
		// numbers of elements we can't simply return an unknown value of the
		// same type since we have not expanded any instances during
		// validation.
		//
		// In order to validate the expression a little precisely, we'll create
		// an unknown map or list here to get more type information.
		switch {
		case config.Count != nil:
			ret = cty.UnknownVal(cty.List(ty))
		case config.ForEach != nil:
			ret = cty.UnknownVal(cty.Map(ty))
		default:
			ret = cty.UnknownVal(ty)
		}
	}

	return ret, diags
}

func (d *evaluationStateData) getResourceSchema(addr addrs.Resource, providerAddr addrs.AbsProviderConfig) *configschema.Block {
	schemas := d.Evaluator.Schemas
	schema, _ := schemas.ResourceTypeConfig(providerAddr.Provider, addr.Mode, addr.Type)
	return schema
}

func (d *evaluationStateData) GetTerraformAttr(addr addrs.TerraformAttr, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	switch addr.Name {

	case "workspace":
		workspaceName := d.Evaluator.Meta.Env
		return cty.StringVal(workspaceName), diags

	case "env":
		// Prior to Terraform 0.12 there was an attribute "env", which was
		// an alias name for "workspace". This was deprecated and is now
		// removed.
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Invalid "terraform" attribute`,
			Detail:   `The terraform.env attribute was deprecated in v0.10 and removed in v0.12. The "state environment" concept was rename to "workspace" in v0.12, and so the workspace name can now be accessed using the terraform.workspace attribute.`,
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags

	default:
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Invalid "terraform" attribute`,
			Detail:   fmt.Sprintf(`The "terraform" object does not have an attribute named %q. The only supported attribute is terraform.workspace, the name of the currently-selected workspace.`, addr.Name),
			Subject:  rng.ToHCL().Ptr(),
		})
		return cty.DynamicVal, diags
	}
}

// nameSuggestion tries to find a name from the given slice of suggested names
// that is close to the given name and returns it if found. If no suggestion
// is close enough, returns the empty string.
//
// The suggestions are tried in order, so earlier suggestions take precedence
// if the given string is similar to two or more suggestions.
//
// This function is intended to be used with a relatively-small number of
// suggestions. It's not optimized for hundreds or thousands of them.
func nameSuggestion(given string, suggestions []string) string {
	for _, suggestion := range suggestions {
		dist := levenshtein.Distance(given, suggestion, nil)
		if dist < 3 { // threshold determined experimentally
			return suggestion
		}
	}
	return ""
}

// moduleDisplayAddr returns a string describing the given module instance
// address that is appropriate for returning to users in situations where the
// root module is possible. Specifically, it returns "the root module" if the
// root module instance is given, or a string representation of the module
// address otherwise.
func moduleDisplayAddr(addr addrs.ModuleInstance) string {
	switch {
	case addr.IsRoot():
		return "the root module"
	default:
		return addr.String()
	}
}
