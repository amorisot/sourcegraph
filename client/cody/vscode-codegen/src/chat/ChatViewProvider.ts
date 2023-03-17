import { isError } from 'lodash'
import * as vscode from 'vscode'

import { CODY_ACCESS_TOKEN_SECRET, getAccessToken, SecretStorage } from '../command/secret-storage'
import { updateConfiguration } from '../configuration'
import { VSCodeEditor } from '../editor/vscode-editor'
import { EmbeddingsClient } from '../embeddings/client'
import { LLMIntentDetector } from '../intent-detector/llm-intent-detector'
import { LocalKeywordContextFetcher } from '../keyword-context/local-keyword-context-fetcher'
import { VSCEKeywordContextFetcher } from '../keyword-context/vsce-keyword-context-fetcher'
import { Message } from '../sourcegraph-api'
import { SourcegraphCompletionsClient } from '../sourcegraph-api/completions'
import { SourcegraphGraphQLAPIClient } from '../sourcegraph-api/graphql'
import { TestSupport } from '../test-support'

import { ChatClient } from './chat'
import { renderMarkdown } from './markdown'
import { Transcript } from './prompt'

export interface ChatMessage extends Message {
    displayText: string
    timestamp: string
    contextFiles?: string[]
}

// If the bot message ends with some prefix of the `Human:` stop
// sequence, trim if from the end.
const STOP_SEQUENCE_REGEXP = /(H|Hu|Hum|Huma|Human|Human:)$/

export class ChatViewProvider implements vscode.WebviewViewProvider {
    private transcript: ChatMessage[] = []
    private messageInProgress: ChatMessage | null = null

    private stopCompletionInProgressCallback: (() => void) | null = null
    private webview?: vscode.Webview

    private tosVersion = 0

    constructor(
        private extensionPath: string,
        private prompt: Transcript,
        private chat: ChatClient,
        private secretStorage: SecretStorage,
        private mode: 'development' | 'production'
    ) {
        if (TestSupport.instance) {
            TestSupport.instance.chatViewProvider.set(this)
        }
    }

    static async create(
        extensionPath: string,
        codebase: string,
        serverEndpoint: string,
        contextType: 'embeddings' | 'keyword' | 'none' | 'blended',
        debug: boolean,
        secretStorage: SecretStorage
    ): Promise<ChatViewProvider> {
        const mode = debug ? 'development' : 'production'
        const accessToken = await getAccessToken(secretStorage)
        const client = new SourcegraphGraphQLAPIClient(serverEndpoint, accessToken)
        const completions = new SourcegraphCompletionsClient(serverEndpoint, accessToken, mode)

        const repoId = codebase ? await client.getRepoId(codebase) : null
        if (isError(repoId)) {
            console.error('error fetching repo id', repoId)
        }

        const embeddings = repoId && !isError(repoId) ? new EmbeddingsClient(client, repoId) : null
        const prompt = new Transcript(
            contextType,
            embeddings,
            new LLMIntentDetector(completions),
            debug ? new VSCEKeywordContextFetcher() : new LocalKeywordContextFetcher('rg'),
            new VSCodeEditor()
        )
        return new ChatViewProvider(extensionPath, prompt, new ChatClient(completions), secretStorage, mode)
    }

    private async onDidReceiveMessage(message: any, webview: vscode.Webview): Promise<void> {
        switch (message.command) {
            case 'initialized':
                await Promise.all([
                    this.sendTranscript(),
                    webview?.postMessage({
                        type: 'token',
                        value: await getAccessToken(this.secretStorage),
                        mode: this.mode,
                    }),
                ])
                break
            case 'reset':
                await this.onResetChat()
                break
            case 'submit':
                await this.onHumanMessageSubmitted(message.text)
                break
            case 'executeRecipe':
                await vscode.commands.executeCommand('cody.chat.focus')
                await this.executeRecipe(message.recipe)
                break
            case 'acceptTOS':
                await this.acceptTOS(message.version)
                break
            case 'setToken':
                await this.secretStorage.store(CODY_ACCESS_TOKEN_SECRET, message.value)
                break
            case 'settings':
                await updateConfiguration('serverEndpoint', message.serverEndpoint)
                await this.secretStorage.store(CODY_ACCESS_TOKEN_SECRET, message.accessToken)
                break
            case 'removeToken':
                await this.secretStorage.delete(CODY_ACCESS_TOKEN_SECRET)
                break
            case 'links':
                await vscode.env.openExternal(vscode.Uri.parse(message.value))
                break
            case 'file':
                await vscode.workspace.openTextDocument(message.value)
            default:
                console.error('Invalid request type from Webview')
        }
    }

    private async acceptTOS(version: number): Promise<void> {
        this.tosVersion = version
        await vscode.commands.executeCommand('cody.accept-tos', version)
    }

    private async sendPrompt(promptMessages: Message[], responsePrefix = ''): Promise<void> {
        this.stopCompletionInProgress()

        this.stopCompletionInProgressCallback = this.chat.chat(promptMessages, {
            onChange: text => this.onBotMessageChange(this.reformatBotMessage(text, responsePrefix)),
            onComplete: () => this.onBotMessageComplete(),
            onError: err => {
                vscode.window.showErrorMessage(err)
            },
        })
    }

    private stopCompletionInProgress(): void {
        this.stopCompletionInProgressCallback?.()
        this.stopCompletionInProgressCallback = null
    }

    private async onResetChat(): Promise<void> {
        this.stopCompletionInProgress()
        this.messageInProgress = null
        this.transcript = []
        this.prompt.reset()
        await this.sendTranscript()
    }

    private async onNewMessageSubmitted(text: string): Promise<void> {
        this.messageInProgress = {
            speaker: 'assistant',
            text: '',
            displayText: '',
            timestamp: getShortTimestamp(),
        }

        this.transcript.push({
            speaker: 'human',
            text,
            displayText: renderMarkdown(text),
            timestamp: getShortTimestamp(),
        })

        await this.sendTranscript()
    }

    private async onHumanMessageSubmitted(text: string): Promise<void> {
        if (this.messageInProgress) {
            return
        }
        await this.onNewMessageSubmitted(text)
        const prompt = await this.prompt.addHumanMessage(text)
        await this.sendPrompt(prompt)
    }

    public async executeRecipe(recipeId: string): Promise<void> {
        if (this.messageInProgress) {
            await vscode.window.showErrorMessage(
                'Cannot execute multiple recipes. Please wait for the current recipe to finish.'
            )
        }

        const messageInfo = await this.prompt.resetToRecipe(recipeId)
        if (!messageInfo) {
            console.error('unrecognized recipe prompt:', recipeId)
            return
        }
        const { display, prompt, botResponsePrefix } = messageInfo

        await this.showTab('ask')

        this.messageInProgress = {
            speaker: 'assistant',
            text: '',
            displayText: '',
            timestamp: getShortTimestamp(),
        }
        this.transcript.push(
            ...display.map(({ speaker, text }) => ({
                speaker,
                text,
                displayText: renderMarkdown(text),
                timestamp: getShortTimestamp(),
            }))
        )

        await this.sendTranscript()

        return this.sendPrompt(prompt, botResponsePrefix)
    }

    private reformatBotMessage(text: string, prefix: string): string {
        let reformattedMessage = prefix + text.trimEnd()

        const stopSequenceMatch = reformattedMessage.match(STOP_SEQUENCE_REGEXP)
        if (stopSequenceMatch) {
            reformattedMessage = reformattedMessage.slice(0, stopSequenceMatch.index)
        }
        // TODO: Detect if bot sent unformatted code without a markdown block.
        return fixOpenMarkdownCodeBlock(reformattedMessage)
    }

    private onBotMessageChange(text: string): void {
        this.messageInProgress = {
            speaker: 'assistant',
            text,
            displayText: renderMarkdown(text),
            timestamp: getShortTimestamp(),
            contextFiles: this.prompt.getLastContextFiles(),
        }

        this.sendTranscript().catch(error => console.error(error))
    }

    private async onBotMessageComplete(): Promise<void> {
        if (this.messageInProgress) {
            this.transcript.push({
                speaker: 'assistant',
                text: this.messageInProgress.text,
                displayText: this.messageInProgress.displayText,
                timestamp: getShortTimestamp(),
                contextFiles: this.prompt.getLastContextFiles(),
            })
            this.prompt.addBotMessage(this.messageInProgress.text)
        }

        this.messageInProgress = null
        this.stopCompletionInProgressCallback = null

        await this.sendTranscript()
    }

    private async showTab(tab: string): Promise<void> {
        await this.webview?.postMessage({ type: 'showTab', tab })
    }

    private async sendTranscript(): Promise<void> {
        await this.webview?.postMessage({
            type: 'transcript',
            messages: this.transcript,
            messageInProgress: this.messageInProgress,
        })
    }

    public async resolveWebviewView(
        webviewView: vscode.WebviewView,
        // eslint-disable-next-line @typescript-eslint/no-unused-vars
        _context: vscode.WebviewViewResolveContext<unknown>,
        // eslint-disable-next-line @typescript-eslint/no-unused-vars
        _token: vscode.CancellationToken
    ): Promise<void> {
        this.webview = webviewView.webview
        const extensionPath = vscode.Uri.parse(this.extensionPath)
        const webviewPath = vscode.Uri.joinPath(extensionPath, 'dist')

        webviewView.webview.options = {
            enableScripts: true,
            localResourceRoots: [webviewPath],
        }

        // await vscode.commands.executeCommand('cody.get-accepted-tos-version')

        //   Create Webview
        const root = vscode.Uri.joinPath(webviewPath, 'index.html')
        const bytes = await vscode.workspace.fs.readFile(root)
        const decoded = new TextDecoder('utf-8').decode(bytes)
        const resources = webviewView.webview.asWebviewUri(webviewPath)

        const nonce = getNonce()

        webviewView.webview.html = decoded
            .replaceAll('./', `${resources.toString()}/`)
            .replace('/nonce/', nonce)
            .replace('/tos-version/', this.tosVersion.toString())

        webviewView.webview.onDidReceiveMessage(message => this.onDidReceiveMessage(message, webviewView.webview))
    }

    public transcriptForTesting(testing: TestSupport): ChatMessage[] {
        if (!testing) {
            console.error('used ForTesting method without test support object')
            return []
        }
        return this.transcript
    }

    public async onConfigChange(change: string, codebase: string, serverEndpoint: string): Promise<void> {
        switch (change) {
            case 'token':
            case 'endpoint': {
                const accessToken = await getAccessToken(this.secretStorage)
                const client = new SourcegraphGraphQLAPIClient(serverEndpoint, accessToken)

                const repoId = codebase ? await client.getRepoId(codebase) : null
                const embeddings = repoId && !isError(repoId) ? new EmbeddingsClient(client, repoId) : null
                this.prompt.setEmbeddings(embeddings)

                const completions = new SourcegraphCompletionsClient(serverEndpoint, accessToken, this.mode)
                this.prompt.setIntentDetector(new LLMIntentDetector(completions))
                this.chat = new ChatClient(completions)

                vscode.window.showInformationMessage('Cody configuration has been updated.')
                break
            }
        }
    }
}

function fixOpenMarkdownCodeBlock(text: string): string {
    const occurances = text.split('```').length - 1
    if (occurances % 2 === 1) {
        return text + '\n```'
    }
    return text
}

function padTimePart(timePart: number): string {
    return timePart < 10 ? `0${timePart}` : timePart.toString()
}

function getShortTimestamp(): string {
    const date = new Date()
    return `${padTimePart(date.getHours())}:${padTimePart(date.getMinutes())}`
}

function getNonce(): string {
    let text = ''
    const possible = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
    for (let i = 0; i < 32; i++) {
        text += possible.charAt(Math.floor(Math.random() * possible.length))
    }
    return text
}
