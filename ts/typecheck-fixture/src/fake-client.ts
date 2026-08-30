// A generated client, faked by hand the way a test does it.
//
// This file exists because the mistake it catches is invisible to everything
// else. A golden test proves the generator emits the bytes it emitted last time,
// and `tsc` over the generated output proves that output compiles — neither one
// asks whether somebody *outside* the client can write a stand-in for it. A
// resource typed as a class with a private field cannot be stood in for at all,
// and nothing in the repository would have said so.
//
// So the claim is asserted here, in the shape a test writes: every resource is
// an interface, so an object satisfies it, and a real client is spread rather
// than rebuilt because `Client.runtime` is a real `Runtime` and `createClient`
// does no I/O.

import { ErrorCode, FieldCode, RigError } from "@rig-ts/client";

import {
    createClient,
    isTodoCreateError,
    type Client,
    type Todo,
    type TodoClient,
    type TodoListResponse,
} from "../../../examples/linearlite/web/src/api/index.js";

declare const todo: Todo;

const page: TodoListResponse = {
    data: [todo],
    pagination: { offset: 0, limit: 20, total: 1 },
};

// The whole resource, written from nothing. This is what a test does when it
// wants no real client in reach at all.
//
// A literal rather than a `declare const` of the type: a declaration constructs
// nothing, so it would satisfy a nominally typed class as readily as an
// interface and assert nothing about which one this is.
const whole: TodoClient = {
    list: async () => page,
    create: async () => todo,
    search: async () => page,
    listDeleted: async () => page,
    delete: async () => {},
    get: async () => todo,
    update: async () => todo,
    claim: async () => todo,
    restore: async () => todo,
    revert: async () => todo,
    versions: async () => page,
};

export function replaced(): Client {
    const client = createClient({ baseUrl: "" });
    return { ...client, todos: whole };
}

// And the partial case, which is the one a test actually writes: the real
// resource supplies every member, and only the call under test is replaced.
//
// It compiles only because a resource's methods are its own properties. A class
// keeps them on the prototype, where a spread cannot see them, and this would
// silently produce an object missing every method it did not name.
export function overridden(): Client {
    const client = createClient({ baseUrl: "" });

    return {
        ...client,
        todos: {
            ...client.todos,
            list: async () => page,
        },
    };
}

// A refusal raised by a double rather than by a server, and read back by the
// generated guard. Both halves are here because both are easy to get wrong from
// the outside: `RigError` takes one object, and the guards read the code rather
// than the status, so a 422 with no code is an error no guard answers for.
export function refused(): Client {
    const client = createClient({ baseUrl: "" });

    return {
        ...client,
        todos: {
            ...client.todos,
            create: async () => {
                throw new RigError({
                    status: 422,
                    code: ErrorCode.UnprocessableEntity,
                    fields: {
                        title: {
                            code: FieldCode.CannotBeEmpty,
                            message: "required",
                        },
                    },
                });
            },
        },
    };
}

export function readBack(err: unknown): string | undefined {
    return isTodoCreateError(err) ? err.fields?.title?.message : undefined;
}
