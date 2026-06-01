# Documentation style

Follow this guidance when writing or editing documentation in this folder,
especially MDX files.

## Brand and naming
- In documentation and readme files, always use "the Firebolt Operator" over "the operator" or "the Kubernetes operator" or "the Firebolt Kubernetes Operator".

## CRD reference accuracy
- The CRD reference pages under `docs/crd-reference/` (`instance-crd-reference.mdx`, `engine-crd-reference.mdx`, `fireboltengineclass-crd-reference.mdx`) describe the CRD spec, status, phases, conditions, short names, and `kubectl` printer columns. They are hand-maintained and not generated.
- When the CRD API types change, you must update the matching reference page in the same change. Triggers include any edit to `api/v1alpha1/*_types.go` (new, renamed, or removed spec or status fields, defaults, validation, immutability), changes to `// +kubebuilder:printcolumn` markers or short names, and new phases, conditions, or reconciler-managed resources.
- Keep the documented short names and example `kubectl get` output in sync with the live CRDs: `fire` for FireboltInstance, `fireng` for FireboltEngine, and `firengc` for FireboltEngineClass.

## Core style guides

- Follow the Google developer documentation style guide.
- Follow the Mintlify style guide.

## Tone and content

- Be conversational and friendly without being frivolous.
- Don't pre-announce anything in documentation.
- Use descriptive link text.
- Write accessibly.
- Write for a global audience.

## Language and grammar

- Use second person: "you" rather than "we."
- Use active voice and make clear who's performing the action.
- Use standard American spelling and punctuation.
- Put conditions before instructions, not after.
- Use terms consistently.

## Formatting, punctuation, and organization

- Use sentence case for document titles and section headings.
- Use numbered lists for sequences.
- Use bulleted lists for most other lists.
- Use description lists for pairs of related pieces of data.
- Use serial commas.
- Put code-related text in code font.
- Put UI elements in bold.
- Use unambiguous date formatting.
- Do not use em-dashes.
- Use periods over semicolons. Rather have two short sentences than one with a semicolon in-between.

## Images

- Provide alt text.
- Provide high-resolution or vector images when practical.

## Mintlify writing principles

- Be concise. People read docs to achieve a goal, so cut unnecessary words.
- Choose clarity over cleverness. Be simple, direct, and avoid jargon or complex sentence structure.
- Use active voice. For example, write "Create a configuration file" instead of "A configuration file should be created."
- Make content skimmable with headings and short paragraphs.
- Write in second person so the documentation is oriented around the reader.

## Common writing mistakes

- Avoid spelling and grammar mistakes.
- Avoid inconsistent terminology, such as switching between "API key" and "API token."
- Avoid product-centric terminology. Orient language around the user's familiarity and task.
- Avoid "Duh" documentation. Don't tell users "Click Save to save."
- Avoid colloquialisms, especially because they hurt localization and clarity.
