import { IEditorInput } from "vs/platform/editor/common/editor";
import { ServiceCollection } from "vs/platform/instantiation/common/serviceCollection";
import { EditorPart } from "vs/workbench/browser/parts/editor/editorPart";
import { IWorkbenchEditorService } from "vs/workbench/services/editor/common/editorService";

import { getBlobPropsFromRouter, router } from "sourcegraph/app/router";
import { urlToBlob } from "sourcegraph/blob/routes";
import { URIUtils } from "sourcegraph/core/uri";
import { getResource } from "sourcegraph/workbench/utils";

// syncEditorWithURL forces the editor model to match current URL blob properties.
// It only needs to be called in an 'onpopstate' handler, for browser forward & back.
export function syncEditorWithRouter(services: ServiceCollection): void {
	const {repo, rev, path} = getBlobPropsFromRouter();
	const resource = URIUtils.pathInRepo(repo, rev, path);
	const editorService = services.get(IWorkbenchEditorService) as IWorkbenchEditorService;
	editorService.openEditor({ resource });
}

// registerEditorCallbacks attaches custom Sourcegraph handling to the workbench editor lifecycle.
export function registerEditorCallbacks(editor: EditorPart): void {
	const stacks = editor.getStacksModel();
	stacks.activeGroup.onEditorActivated(editorOpened);
}

// editorOpened is called whenever a new editor is created or activated. E.g:
//  - on page load
//  - from file explorer
//  - for a cross-file j2d
function editorOpened(input: IEditorInput): void {
	let {repo, rev, path} = URIUtils.repoParams(getResource(input));
	if (rev === "HEAD") {
		rev = null;
	}
	router.push(urlToBlob(repo, rev, path));
}

