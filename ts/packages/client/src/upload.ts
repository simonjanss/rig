/** One file a caller is sending. */
export type Upload = {
    /**
     * What the file is called. The server records it on the row and puts it in
     * the download path, and it never becomes the storage key — so a name with a
     * slash in it is a strange name and not a way out of the bucket.
     */
    name: string;

    /** The bytes. A `File` from an `<input type="file">` is already one of these. */
    body: Blob;

    /**
     * What the caller claims the bytes are. Absent takes the blob's own type,
     * and it makes little difference either way: the server sniffs the content,
     * and the sniffed type is the one it stores and the one it serves back.
     */
    contentType?: string;
};

/** The part the row travels in. The server reads the parts in order and needs
 * the row before it has anywhere to put the bytes. */
const JSON_PART = "json";

/**
 * Builds the `multipart/form-data` body rig's upload endpoints take: the row,
 * and the files beside it.
 *
 * It is the shape rig's own endpoints take rather than a general form encoder. A
 * generated method calls it; a caller supplies the {@link Upload}s.
 *
 * `row` is left out entirely when absent, which is what a bare upload does:
 * there is no row to send, only bytes for a row that already exists.
 *
 * An absent upload is left out the same way, so a generated method can name
 * every file column of a create unconditionally. What makes a required one
 * impossible to leave out is the generated shape it arrives in — the member is
 * optional there only where the column is nullable — and not a check here,
 * which would report at runtime what the compiler has already refused.
 */
export function multipart(
    row: unknown,
    files: ReadonlyArray<readonly [field: string, upload: Upload | undefined]>,
): FormData {
    const form = new FormData();

    // Written first, because the server binds the bytes to a row it has to have
    // read already.
    if (row !== undefined) {
        form.append(
            JSON_PART,
            new Blob([JSON.stringify(row)], { type: "application/json" }),
        );
    }

    for (const [field, upload] of files) {
        if (upload === undefined) continue;
        const body =
            upload.contentType === undefined
                ? upload.body
                : new Blob([upload.body], { type: upload.contentType });
        form.append(field, body, upload.name);
    }

    return form;
}
